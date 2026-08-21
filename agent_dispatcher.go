package claudeacp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/lifecycle"
)

const postResponseHookIDParam = "_acp_go_claude_post_response_hook_id"

type localAgentConnection struct {
	agent       *Agent
	conn        *acp.Connection
	initialized atomic.Bool
	hooks       *postResponseHooks
}

type localAgentHandler func(context.Context, *Agent, json.RawMessage) (any, *acp.RequestError)

type localAgentParams[Req any] interface {
	*Req
	Validate() error
}

var (
	_ agentClient = (*localAgentConnection)(nil)

	localAgentHandlers = map[string]localAgentHandler{
		acp.AgentMethodAuthenticate:           localResponse((*Agent).Authenticate),
		acp.AgentMethodInitialize:             localResponse((*Agent).Initialize),
		acp.AgentMethodLogout:                 localResponse((*Agent).Logout),
		acp.AgentMethodSessionCancel:          localNotification((*Agent).Cancel),
		acp.AgentMethodSessionClose:           localResponse((*Agent).CloseSession),
		acp.AgentMethodSessionDelete:          localResponse((*Agent).UnstableDeleteSession),
		acp.AgentMethodSessionList:            localResponse((*Agent).ListSessions),
		acp.AgentMethodSessionLoad:            localResponse((*Agent).LoadSession),
		acp.AgentMethodSessionNew:             localResponse((*Agent).NewSession),
		acp.AgentMethodSessionPrompt:          localResponse((*Agent).Prompt),
		acp.AgentMethodSessionResume:          localResponse((*Agent).ResumeSession),
		acp.AgentMethodSessionSetConfigOption: localResponse((*Agent).SetSessionConfigOption),
	}
)

func newLocalAgentConnection(agent *Agent, output io.Writer, input io.Reader) *localAgentConnection {
	writes := newHostWriteOwner(output)
	agent.setLifecycleCarrier(writes.interruptible())
	hooks := &postResponseHooks{log: agent.log, writes: writes}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	inputGate := newConnectionInputGate(newPostResponseHookRequestReader(input))
	conn.conn = acp.NewConnection(conn.handle, hooks.wrap(output), inputGate)
	conn.conn.SetLogger(agent.log)
	inputGate.open()

	return conn
}

type connectionInputGate struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

// connectionInputGate blocks the SDK receive goroutine until the connection
// logger is installed. The SDK starts receiving inside NewConnection.
func newConnectionInputGate(reader io.Reader) *connectionInputGate {
	return &connectionInputGate{
		reader: reader,
		ready:  make(chan struct{}),
	}
}

func (g *connectionInputGate) open() {
	g.once.Do(func() {
		close(g.ready)
	})
}

func (g *connectionInputGate) Read(p []byte) (int, error) {
	<-g.ready

	return g.reader.Read(p)
}

func (c *localAgentConnection) Done() <-chan struct{} {
	return c.conn.Done()
}

func (c *localAgentConnection) handle(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	if err := c.agent.ensureOpen(); err != nil {
		return nil, requestError(ctx, err)
	}

	if method != acp.AgentMethodInitialize && !c.initialized.Load() {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldMethod: method,
			jsonFieldError:  "initialize must be called before other ACP methods",
		})
	}

	if strings.HasPrefix(method, "_") {
		result, err := c.agent.HandleExtensionMethod(ctx, method, params)

		reqErr := requestError(ctx, err)
		if reqErr == nil {
			c.enqueueSessionEstablishedHook(ctx, method, params, result)
		}

		return result, reqErr
	}

	handler, ok := localAgentHandlers[method]
	if !ok {
		return nil, acp.NewMethodNotFound(method)
	}

	result, reqErr := handler(ctx, c.agent, params)
	if method == acp.AgentMethodInitialize && reqErr == nil {
		c.initialized.Store(true)
	}

	if reqErr == nil {
		c.enqueueSessionEstablishedHook(ctx, method, params, result)
	}

	return result, reqErr
}

// enqueueSessionEstablishedHook runs the work a newly established session owes
// its host once the establishing response has been written to the transport: the
// lifecycle incarnation's opening snapshot, which must be that session's first
// lifecycle-bearing notification, and then its command catalog.
func (c *localAgentConnection) enqueueSessionEstablishedHook(ctx context.Context, method string, params json.RawMessage, result any) {
	sessionID, ok := establishedSessionID(method, params, result)
	if !ok || c.hooks == nil {
		return
	}

	responseID := postResponseHookRequestID(params)
	if responseID == "" {
		return
	}

	session, err := c.agent.session(sessionID)
	if err != nil {
		c.logSessionEstablishedFailure(context.WithoutCancel(ctx), "session lookup", method, sessionID)

		return
	}

	finishProducer, admitted := session.producers.begin()
	if !admitted {
		return
	}

	c.hooks.enqueue(responseID, func() {
		defer finishProducer()

		hookCtx := context.WithoutCancel(ctx)

		if err := session.completeEstablishment(hookCtx); err != nil {
			c.logSessionEstablishedFailure(hookCtx, "establishment publication", method, sessionID)
		}
	}, finishProducer)
}

func (c *localAgentConnection) logSessionEstablishedFailure(
	ctx context.Context,
	stage string,
	method string,
	sessionID acp.SessionId,
) {
	c.agent.log.ErrorContext(ctx, "post-response session establishment failed",
		slog.String("stage", stage),
		slog.String(jsonFieldMethod, method),
		slog.String(acpFieldSessionID, string(sessionID)),
		slog.String("classification", "lifecycle_boundary"),
	)
}

func establishedSessionID(method string, params json.RawMessage, result any) (acp.SessionId, bool) {
	switch method {
	case acp.AgentMethodSessionNew:
		resp, ok := result.(acp.NewSessionResponse)

		return resp.SessionId, ok && resp.SessionId != ""
	case acp.AgentMethodSessionLoad:
		var req acp.LoadSessionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return "", false
		}

		return req.SessionId, req.SessionId != ""
	case acp.AgentMethodSessionResume:
		var req acp.ResumeSessionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return "", false
		}

		return req.SessionId, req.SessionId != ""
	case ForkSessionMethod:
		resp, ok := result.(acp.UnstableForkSessionResponse)

		return resp.SessionId, ok && resp.SessionId != ""
	default:
		return "", false
	}
}

type postResponseHookRequestReader struct {
	reader     *bufio.Reader
	pending    []byte
	pendingErr error
}

func newPostResponseHookRequestReader(reader io.Reader) *postResponseHookRequestReader {
	return &postResponseHookRequestReader{reader: bufio.NewReader(reader)}
}

func (r *postResponseHookRequestReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.pendingErr != nil {
			err := r.pendingErr
			r.pendingErr = nil

			return 0, err
		}

		line, err := r.reader.ReadBytes('\n')
		if len(line) == 0 {
			return 0, err
		}

		r.pending = tagPostResponseHookRequest(line)
		if err != nil {
			r.pendingErr = err
		}
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]

	return n, nil
}

func tagPostResponseHookRequest(line []byte) []byte {
	var msg struct {
		JSONRPC string           `json:"jsonrpc,omitempty"`
		ID      *json.RawMessage `json:"id,omitempty"`
		Method  string           `json:"method,omitempty"`
		Params  json.RawMessage  `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return line
	}

	if msg.ID == nil || !lifecycleCommandMethod(msg.Method) || len(bytes.TrimSpace(msg.Params)) == 0 {
		return line
	}

	params := make(map[string]json.RawMessage)
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return line
	}

	if params == nil {
		return line
	}

	hookID, _ := json.Marshal(responseHookID(msg.ID))
	params[postResponseHookIDParam] = hookID
	msg.Params, _ = json.Marshal(params)

	tagged, _ := json.Marshal(msg)
	if bytes.HasSuffix(line, []byte("\n")) {
		tagged = append(tagged, '\n')
	}

	return tagged
}

func lifecycleCommandMethod(method string) bool {
	switch method {
	case acp.AgentMethodSessionNew, acp.AgentMethodSessionLoad, acp.AgentMethodSessionResume, ForkSessionMethod:
		return true
	default:
		return false
	}
}

// postResponseHookRequestID reads back the tag the request reader stamped. Only
// the tag is a string: a real establishing request carries the mandatory
// mcpServers array and an object-valued _meta beside it, so the params are
// decoded as raw values and only the tag is read as one. Decoding them all as
// strings would fail on every request this hook exists to serve.
func postResponseHookRequestID(params json.RawMessage) string {
	var tagged map[string]json.RawMessage
	if err := json.Unmarshal(params, &tagged); err != nil {
		return ""
	}

	raw, present := tagged[postResponseHookIDParam]
	if !present {
		return ""
	}

	var id string

	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}

	return id
}

type postResponseHooks struct {
	log          *slog.Logger
	writes       *hostWriteOwner
	mu           sync.Mutex
	all          []postResponseHook
	actionWrites map[actionRequestCorrelation]*actionRequestWrite
	writeGate    chan struct{}
	activeWrite  *sdkWriteOwnership
	gateOnce     sync.Once
}

type sdkWriteOwnership struct {
	method    string
	completed bool
	begun     bool
	canceled  atomic.Bool
	stop      func() bool
	joined    chan struct{}
}

type postResponseHook struct {
	responseID string
	run        func()
	cancel     func()
}

type actionRequestWrite struct {
	begun  chan struct{}
	result chan actionRequestWriteResult
}

type actionRequestWriteResult struct {
	identity         actionWireIdentity
	written          bool
	requestCompleted bool
}

// actionRequestCorrelation is the closed, exact registration identity of one
// outbound permission or elicitation. Hashing the complete method parameters
// lets the writer match the request it actually receives without exposing or
// adding metadata, including when lifecycle envelopes were not negotiated.
type actionRequestCorrelation struct {
	method     string
	paramsHash [sha256.Size]byte
}

var (
	errActionWireRegistration = errors.New("action wire registration failed")
	errActionWireWrite        = errors.New("action request write failed")
	errHostWriteAborted       = errors.New("active host write aborted for containment")
	errHostWrite              = errors.New("host transport write failed")
	errHostWriterUnsupported  = errors.New("host writer is not interruptible")
	errHostWriterClosed       = errors.New("host writer is closed")
)

func (h *postResponseHooks) wrap(writer io.Writer) io.Writer {
	if h.writes == nil {
		h.writes = newHostWriteOwner(writer)
	}

	return &postResponseWriter{writer: h.writes, hooks: h}
}

func (h *postResponseHooks) enqueue(responseID string, run func(), cancel ...func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var cancelHook func()
	if len(cancel) != 0 {
		cancelHook = cancel[0]
	}

	h.all = append(h.all, postResponseHook{
		responseID: responseID,
		run:        run,
		cancel:     cancelHook,
	})
}

// cancelPending releases hooks whose establishing response never crossed the
// host-write boundary. A hook already removed owns its admitted worker and is
// joined by the session producer gate; one still present owns no response and
// must surrender that admission before close waits on the gate.
func (h *postResponseHooks) cancelPending() {
	h.mu.Lock()
	pending := h.all
	h.all = nil
	h.mu.Unlock()

	for _, hook := range pending {
		if hook.cancel != nil {
			hook.cancel()
		}
	}
}

func (h *postResponseHooks) registerActionWrite(correlation actionRequestCorrelation) (*actionRequestWrite, error) {
	if h.writes == nil || !h.writes.interruptible() {
		return nil, errHostWriterUnsupported
	}

	if h.writes.closed() {
		return nil, errHostWriterClosed
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.actionWrites == nil {
		h.actionWrites = make(map[actionRequestCorrelation]*actionRequestWrite)
	}

	if _, exists := h.actionWrites[correlation]; exists {
		return nil, errActionWireRegistration
	}

	registration := &actionRequestWrite{
		begun:  make(chan struct{}),
		result: make(chan actionRequestWriteResult, 1),
	}
	h.actionWrites[correlation] = registration

	return registration, nil
}

// interruptActiveWrite atomically distinguishes a completed full write from a
// write whose context-free host writer still owns the callback. Only the latter
// closes the transport. The close prevents future admissions and the wait joins
// the exact Write invocation before callback ownership can settle.
func (h *postResponseHooks) interruptActiveWrite() error {
	if h.writes == nil {
		return nil
	}

	return h.writes.interruptActive()
}

func (h *postResponseHooks) closeWrites() error {
	if h.writes == nil {
		return nil
	}

	return h.writes.close()
}

func (h *postResponseHooks) beginSDKWrite(
	ctx context.Context,
	method string,
	watchCancellation bool,
) (*sdkWriteOwnership, error) {
	h.gateOnce.Do(func() {
		h.writeGate = make(chan struct{}, 1)
		h.writeGate <- struct{}{}
	})

	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-h.writeGate:
	}

	ownership := &sdkWriteOwnership{method: method}

	h.mu.Lock()
	h.activeWrite = ownership
	h.mu.Unlock()

	if watchCancellation && h.writes != nil && h.writes.interruptible() {
		ownership.joined = make(chan struct{})
		ownership.stop = context.AfterFunc(ctx, func() {
			defer close(ownership.joined)

			ownership.canceled.Store(true)

			h.mu.Lock()
			pending := h.activeWrite == ownership && !ownership.completed
			begun := ownership.begun
			h.mu.Unlock()

			if !pending {
				return
			}

			if begun {
				_ = h.interruptActiveWrite()

				return
			}

			_ = h.writes.abort()
		})
	}

	return ownership, nil
}

func (h *postResponseHooks) finishSDKCall(ownership *sdkWriteOwnership) {
	if ownership.stop != nil && !ownership.stop() {
		<-ownership.joined
	}

	h.mu.Lock()
	if h.activeWrite == ownership && !ownership.completed {
		ownership.completed = true
		h.activeWrite = nil
		h.releaseSDKWriteLocked()
	}
	h.mu.Unlock()
}

func (h *postResponseHooks) beginSDKLine(data []byte) *sdkWriteOwnership {
	var message struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &message); err != nil || message.Method == "" {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	ownership := h.activeWrite
	if ownership == nil || ownership.method != message.Method || ownership.completed {
		return nil
	}

	ownership.begun = true

	return ownership
}

func (h *postResponseHooks) finishSDKLine(ownership *sdkWriteOwnership) {
	if ownership == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.activeWrite != ownership || ownership.completed {
		return
	}

	ownership.completed = true
	h.activeWrite = nil
	h.releaseSDKWriteLocked()
}

func (h *postResponseHooks) releaseSDKWriteLocked() {
	h.writeGate <- struct{}{}
}

func (h *postResponseHooks) unregisterActionWrite(
	correlation actionRequestCorrelation,
	registration *actionRequestWrite,
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.actionWrites[correlation] == registration {
		delete(h.actionWrites, correlation)
	}
}

// withdrawActionWrite removes a request that has not reached the writer. The
// writer uses the same lock to begin a correlated line, so true proves any late
// attempt will be rejected before a byte reaches the host.
func (h *postResponseHooks) withdrawActionWrite(
	correlation actionRequestCorrelation,
	registration *actionRequestWrite,
) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.actionWrites[correlation] != registration {
		return false
	}

	select {
	case <-registration.begun:
		return false
	default:
		delete(h.actionWrites, correlation)

		return true
	}
}

func (h *postResponseHooks) beginActionWrite(data []byte) (*actionRequestWrite, actionWireIdentity, bool) {
	correlation, identity, ok := actionRequestWireIdentity(data)
	if !ok {
		return nil, actionWireIdentity{}, false
	}

	h.mu.Lock()

	registration := h.actionWrites[correlation]
	if registration == nil {
		h.mu.Unlock()

		return nil, identity, true
	}

	select {
	case <-registration.begun:
		h.mu.Unlock()

		return nil, identity, true
	default:
		close(registration.begun)
	}
	h.mu.Unlock()

	return registration, identity, true
}

func (h *postResponseHooks) finishActionWrite(
	registration *actionRequestWrite,
	identity actionWireIdentity,
	written bool,
) {
	if registration == nil {
		return
	}

	select {
	case registration.result <- actionRequestWriteResult{identity: identity, written: written}:
	default:
	}
}

func actionRequestWireIdentity(data []byte) (actionRequestCorrelation, actionWireIdentity, bool) {
	var message struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &message); err != nil || message.ID == nil {
		return actionRequestCorrelation{}, actionWireIdentity{}, false
	}

	correlation, ok := actionRequestCorrelationFor(message.Method, message.Params)
	if !ok {
		return actionRequestCorrelation{}, actionWireIdentity{}, false
	}

	return correlation, actionWireIdentity{
		method: message.Method, requestID: responseHookID(message.ID),
	}, true
}

func actionRequestCorrelationFor(method string, params any) (actionRequestCorrelation, bool) {
	if method != acp.ClientMethodSessionRequestPermission && method != acp.ClientMethodElicitationCreate {
		return actionRequestCorrelation{}, false
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return actionRequestCorrelation{}, false
	}

	return actionRequestCorrelation{method: method, paramsHash: sha256.Sum256(raw)}, true
}

func lifecycleActionID(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"` //nolint:tagliatelle // Reserved ACP metadata key.
	}

	_ = json.Unmarshal(raw, &params)

	rawCorrelation := params.Meta[lifecycle.MetaKey]

	var correlation struct {
		Action struct {
			ActionID string `json:"actionId"`
		} `json:"action"`
	}
	if err := json.Unmarshal(rawCorrelation, &correlation); err != nil {
		return ""
	}

	return correlation.Action.ActionID
}

func (h *postResponseHooks) runAfterResponseWrite(data []byte) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(data), &msg); err != nil || msg == nil {
		if h.log != nil {
			h.log.Debug("parse response for post-response hook failed", slog.String("stage", "response_decode"))
		}

		return
	}

	rawID, hasID := msg[jsonFieldID]
	_, hasMethod := msg[jsonFieldMethod]
	_, hasResult := msg[jsonFieldResult]
	_, hasError := msg[jsonFieldError]

	var jsonrpc string
	if rawVersion, ok := msg["jsonrpc"]; ok {
		_ = json.Unmarshal(rawVersion, &jsonrpc)
	}

	if !hasID || len(bytes.TrimSpace(rawID)) == 0 || hasMethod || !hasResult || hasError || jsonrpc != "2.0" {
		return
	}

	responseID := string(bytes.TrimSpace(rawID))

	h.mu.Lock()
	for index, hook := range h.all {
		if hook.responseID != responseID {
			continue
		}

		h.all = slicesDelete(h.all, index)
		h.mu.Unlock()

		go runPostResponseHook(h.log, hook.run)

		return
	}
	h.mu.Unlock()
}

func runPostResponseHook(log *slog.Logger, run func()) {
	defer recoverAgentGoroutine(context.Background(), log, "post-response hook")

	run()
}

func responseHookID(id *json.RawMessage) string {
	return string(bytes.TrimSpace(*id))
}

func slicesDelete[S ~[]E, E any](slice S, index int) S {
	return append(slice[:index], slice[index+1:]...)
}

type postResponseWriter struct {
	writer *hostWriteOwner
	hooks  *postResponseHooks
}

func (w *postResponseWriter) Write(data []byte) (int, error) {
	ownership := w.hooks.beginSDKLine(data)

	registration, identity, actionRequest := w.hooks.beginActionWrite(data)
	if actionRequest && registration == nil {
		w.hooks.finishSDKLine(ownership)

		return 0, errActionWireRegistration
	}

	n, err := w.writer.write(data, ownership)
	w.hooks.finishActionWrite(registration, identity, err == nil && n == len(data))
	w.hooks.finishSDKLine(ownership)

	if err != nil {
		return n, &safeStageError{stage: errHostWrite, cause: err}
	}

	w.hooks.runAfterResponseWrite(data)

	return n, nil
}

type safeStageError struct {
	stage error
	cause error
}

func (e *safeStageError) Error() string { return e.stage.Error() }

func (e *safeStageError) Unwrap() []error { return []error{e.stage, e.cause} }

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

// hostWriteOwner is the containment owner for context-free SDK writes. It does
// not put Write in an unjoinable goroutine: the calling goroutine remains the
// owner, while cancellation interrupts the underlying writer and waits for that
// exact call to return. An action request is admitted only when the writer
// exposes a supported interruption boundary.
type hostWriteOwner struct {
	writer io.Writer
	closer io.Closer
	setter writeDeadlineSetter

	mu       sync.Mutex
	required bool
	nextID   uint64
	active   map[uint64]*hostWriteAttempt
	stopped  bool
	done     chan struct{}
	once     sync.Once
	stop     sync.Once
	stopErr  error
}

type hostWriteAttempt struct {
	done chan struct{}
	full bool
}

func newHostWriteOwner(writer io.Writer) *hostWriteOwner {
	owner := &hostWriteOwner{writer: writer, active: make(map[uint64]*hostWriteAttempt), done: make(chan struct{})}
	owner.closer, _ = writer.(io.Closer)
	owner.setter, _ = writer.(writeDeadlineSetter)

	return owner
}

func (w *hostWriteOwner) requireInterruptible() {
	w.mu.Lock()
	w.required = true
	w.mu.Unlock()
}

func (w *hostWriteOwner) interruptible() bool {
	return w != nil && (w.closer != nil || w.setter != nil)
}

func (w *hostWriteOwner) closed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.stopped
}

func (w *hostWriteOwner) Write(data []byte) (int, error) {
	return w.write(data, nil)
}

func (w *hostWriteOwner) write(data []byte, ownership *sdkWriteOwnership) (int, error) {
	w.mu.Lock()
	if w.required && !w.interruptible() {
		w.mu.Unlock()

		return 0, errHostWriterUnsupported
	}

	if w.stopped {
		w.mu.Unlock()

		return 0, errHostWriterClosed
	}

	if ownership != nil && ownership.canceled.Load() {
		w.stopped = true
		w.once.Do(func() { close(w.done) })
		w.mu.Unlock()
		_ = w.interrupt()

		return 0, errHostWriteAborted
	}

	w.nextID++
	id := w.nextID
	attempt := &hostWriteAttempt{done: make(chan struct{})}
	w.active[id] = attempt
	required := w.required
	w.mu.Unlock()

	var timer *time.Timer
	if required {
		timer = time.AfterFunc(defaultSessionSettlementTimeout, (&hostWriteTimeout{owner: w, id: id}).interrupt)
	}

	n, err := w.writer.Write(data)

	if timer != nil {
		timer.Stop()
	}

	w.mu.Lock()
	delete(w.active, id)

	full := err == nil && n == len(data)

	attempt.full = full
	if !full {
		w.stopped = true
	}

	close(attempt.done)

	if w.stopped && len(w.active) == 0 {
		w.once.Do(func() { close(w.done) })
	}
	w.mu.Unlock()

	if !full {
		_ = w.interrupt()
	}

	if !full {
		return n, errors.Join(errHostWriteAborted, err)
	}

	return n, err
}

func (w *hostWriteOwner) interruptExact(id uint64) {
	w.mu.Lock()
	active := w.active[id] != nil
	w.mu.Unlock()

	if active {
		_ = w.interrupt()
	}
}

type hostWriteTimeout struct {
	owner *hostWriteOwner
	id    uint64
}

func (t *hostWriteTimeout) interrupt() {
	t.owner.interruptExact(t.id)
}

func (w *hostWriteOwner) interruptActive() error {
	w.mu.Lock()
	if len(w.active) == 0 {
		w.mu.Unlock()

		return nil
	}

	attempts := make([]*hostWriteAttempt, 0, len(w.active))

	for _, attempt := range w.active {
		attempts = append(attempts, attempt)
	}
	w.mu.Unlock()

	err := w.interrupt()

	for _, attempt := range attempts {
		<-attempt.done
	}

	w.mu.Lock()
	allFull := true

	for _, attempt := range attempts {
		allFull = allFull && attempt.full
	}
	w.mu.Unlock()

	if allFull {
		return nil
	}

	<-w.done

	return err
}

func (w *hostWriteOwner) close() error {
	return w.abort()
}

func (w *hostWriteOwner) abort() error {
	w.mu.Lock()
	w.stopped = true

	active := len(w.active)

	if active == 0 {
		w.once.Do(func() { close(w.done) })
	}
	w.mu.Unlock()

	err := w.interrupt()
	<-w.done

	return err
}

func (w *hostWriteOwner) interrupt() error {
	w.stop.Do(func() {
		var errs []error

		if w.setter != nil {
			if err := w.setter.SetWriteDeadline(time.Now()); err != nil {
				errs = append(errs, errors.New("host write deadline interruption failed"))
			}
		}

		if w.closer != nil {
			if err := w.closer.Close(); err != nil {
				errs = append(errs, errors.New("host writer close interruption failed"))
			}
		}

		w.stopErr = errors.Join(errs...)
	})

	return w.stopErr
}

func localResponse[Req any, ReqPtr localAgentParams[Req], Resp any](
	call func(*Agent, context.Context, Req) (Resp, error),
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		resp, err := call(agent, ctx, value)
		if err != nil {
			return nil, requestError(ctx, err)
		}

		return resp, nil
	}
}

func localNotification[Req any, ReqPtr localAgentParams[Req]](
	call func(*Agent, context.Context, Req) error,
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		if err := call(agent, ctx, value); err != nil {
			return nil, requestError(ctx, err)
		}

		return nil, nil
	}
}

func decodeLocalAgentParams[Req any, ReqPtr localAgentParams[Req]](params json.RawMessage) (Req, *acp.RequestError) {
	var value Req
	if err := json.Unmarshal(params, &value); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := ReqPtr(&value).Validate(); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	return value, nil
}

func (c *localAgentConnection) UnstableCompleteElicitation(
	ctx context.Context,
	params acp.UnstableCompleteElicitationNotification,
) error {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodElicitationComplete, true)
	if err != nil {
		return err
	}
	defer c.hooks.finishSDKCall(write)

	return c.conn.SendNotification(ctx, acp.ClientMethodElicitationComplete, params)
}

func (c *localAgentConnection) CreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
	action actionWireAdmission,
) (acp.UnstableCreateElicitationResponse, error) {
	raw, err := scopedElicitationParams(params, scope)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}
	defer release()

	return sendLifecycleActionRequest[acp.UnstableCreateElicitationResponse](
		c, ctx, acp.ClientMethodElicitationCreate, raw, action,
	)
}

func (c *localAgentConnection) ReadTextFile(
	ctx context.Context,
	params acp.ReadTextFileRequest,
) (acp.ReadTextFileResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodFsReadTextFile, true)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.ReadTextFileResponse](c.conn, ctx, acp.ClientMethodFsReadTextFile, params)
}

func (c *localAgentConnection) WriteTextFile(
	ctx context.Context,
	params acp.WriteTextFileRequest,
) (acp.WriteTextFileResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodFsWriteTextFile, true)
	if err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.WriteTextFileResponse](c.conn, ctx, acp.ClientMethodFsWriteTextFile, params)
}

func (c *localAgentConnection) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
	action actionWireAdmission,
) (acp.RequestPermissionResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	defer release()

	return sendLifecycleActionRequest[acp.RequestPermissionResponse](
		c, ctx, acp.ClientMethodSessionRequestPermission, params, action,
	)
}

type lifecycleActionRequestResult[T any] struct {
	response T
	err      error
}

func sendLifecycleActionRequest[T any](
	c *localAgentConnection,
	ctx context.Context,
	method string,
	params any,
	action actionWireAdmission,
) (T, error) {
	if !action.present() {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}

		return zero, errActionWireRegistration
	}

	// The native tool identity is the ordinary ACP fact the callback exposes.
	// Publish it before the correlated request; lifecycle action visibility waits
	// for the request's exact id and complete transport write below.
	if err := action.publishPending(); err != nil {
		var zero T

		return zero, err
	}

	correlation, ok := actionRequestCorrelationFor(method, params)
	if !ok || (action.actionID != "" && lifecycleActionID(params) != action.actionID) {
		var zero T

		return zero, errActionWireRegistration
	}

	registration, err := c.hooks.registerActionWrite(correlation)
	if err != nil {
		var zero T

		return zero, err
	}
	defer c.hooks.unregisterActionWrite(correlation, registration)

	writeOwnership, err := c.hooks.beginSDKWrite(ctx, method, false)
	if err != nil {
		var zero T

		return zero, err
	}

	requestCtx, cancelRequest := context.WithCancelCause(ctx)
	defer cancelRequest(context.Canceled)

	result := make(chan lifecycleActionRequestResult[T], 1)

	go func() {
		response, requestErr := acp.SendRequest[T](c.conn, requestCtx, method, params)
		c.hooks.finishSDKCall(writeOwnership)

		if requestErr != nil && requestCtx.Err() == nil && c.hooks.withdrawActionWrite(correlation, registration) {
			registration.result <- actionRequestWriteResult{requestCompleted: true}
		}

		result <- lifecycleActionRequestResult[T]{response: response, err: requestErr}
	}()

	var write actionRequestWriteResult
	select {
	case write = <-registration.result:
	case <-ctx.Done():
		cancelRequest(context.Cause(ctx))

		if c.hooks.withdrawActionWrite(correlation, registration) {
			interruptErr := c.hooks.writes.abort()

			<-result

			var zero T

			return zero, errors.Join(context.Cause(ctx), errHostWriteAborted, interruptErr)
		}

		interruptErr := c.hooks.interruptActiveWrite()
		write = <-registration.result

		if !write.written {
			<-result

			var zero T

			return zero, errors.Join(context.Cause(ctx), errHostWriteAborted, interruptErr)
		}
	}

	if !write.written {
		cancelRequest(errActionWireWrite)

		completed := <-result
		if write.requestCompleted {
			return completed.response, completed.err
		}

		var zero T

		return zero, errActionWireWrite
	}

	if err := action.observeWrite(context.WithoutCancel(ctx), write.identity); err != nil {
		cancelRequest(err)
		<-result

		var zero T

		return zero, err
	}

	completed := <-result

	return completed.response, completed.err
}

func (c *localAgentConnection) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodSessionUpdate, true)
	if err != nil {
		return err
	}
	defer c.hooks.finishSDKCall(write)

	return c.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, params)
}

func (c *localAgentConnection) CreateTerminal(
	ctx context.Context,
	params acp.CreateTerminalRequest,
) (acp.CreateTerminalResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.CreateTerminalResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodTerminalCreate, true)
	if err != nil {
		return acp.CreateTerminalResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.CreateTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalCreate, params)
}

func (c *localAgentConnection) KillTerminal(
	ctx context.Context,
	params acp.KillTerminalRequest,
) (acp.KillTerminalResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.KillTerminalResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodTerminalKill, true)
	if err != nil {
		return acp.KillTerminalResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.KillTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalKill, params)
}

func (c *localAgentConnection) TerminalOutput(
	ctx context.Context,
	params acp.TerminalOutputRequest,
) (acp.TerminalOutputResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.TerminalOutputResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodTerminalOutput, true)
	if err != nil {
		return acp.TerminalOutputResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.TerminalOutputResponse](c.conn, ctx, acp.ClientMethodTerminalOutput, params)
}

func (c *localAgentConnection) ReleaseTerminal(
	ctx context.Context,
	params acp.ReleaseTerminalRequest,
) (acp.ReleaseTerminalResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.ReleaseTerminalResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodTerminalRelease, true)
	if err != nil {
		return acp.ReleaseTerminalResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.ReleaseTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalRelease, params)
}

func (c *localAgentConnection) WaitForTerminalExit(
	ctx context.Context,
	params acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.WaitForTerminalExitResponse{}, err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, acp.ClientMethodTerminalWaitForExit, true)
	if err != nil {
		return acp.WaitForTerminalExitResponse{}, err
	}
	defer c.hooks.finishSDKCall(write)

	return acp.SendRequest[acp.WaitForTerminalExitResponse](c.conn, ctx, acp.ClientMethodTerminalWaitForExit, params)
}

func (c *localAgentConnection) NotifyExtension(ctx context.Context, method string, params any) error {
	if method == "" || !strings.HasPrefix(method, "_") {
		return fmt.Errorf("extension method name must start with '_' (got %q)", method)
	}

	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	write, err := c.hooks.beginSDKWrite(ctx, method, true)
	if err != nil {
		return err
	}
	defer c.hooks.finishSDKCall(write)

	return c.conn.SendNotification(ctx, method, params)
}
