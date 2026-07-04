package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/acp-go-sdk"
)

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
	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	inputGate := newConnectionInputGate(input)
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
	if method != acp.AgentMethodInitialize && !c.initialized.Load() {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldMethod: method,
			jsonFieldError:  "initialize must be called before other ACP methods",
		})
	}

	if strings.HasPrefix(method, "_") {
		result, err := c.agent.HandleExtensionMethod(ctx, method, params)

		reqErr := requestError(err)
		if reqErr == nil {
			c.enqueueLifecycleCommandHook(ctx, method, params, result)
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
		c.enqueueLifecycleCommandHook(ctx, method, params, result)
	}

	return result, reqErr
}

func (c *localAgentConnection) enqueueLifecycleCommandHook(ctx context.Context, method string, params json.RawMessage, result any) {
	sessionID, ok := lifecycleCommandSessionID(method, params, result)
	if !ok || c.hooks == nil {
		return
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		c.agent.log.ErrorContext(ctx, "marshal lifecycle response for post-response hook failed",
			slog.String(jsonFieldMethod, method),
			slog.String(jsonFieldError, err.Error()),
		)

		return
	}

	c.hooks.enqueue(resultJSON, func() {
		hookCtx := context.WithoutCancel(ctx)

		session, err := c.agent.session(sessionID)
		if err != nil {
			c.agent.log.ErrorContext(hookCtx, "post-response command update session lookup failed",
				slog.String(jsonFieldMethod, method),
				slog.String(acpFieldSessionID, string(sessionID)),
				slog.String(jsonFieldError, err.Error()),
			)

			return
		}

		if err := session.emitAvailableCommandsUpdate(hookCtx, true); err != nil {
			c.agent.log.ErrorContext(hookCtx, "post-response command update failed",
				slog.String(jsonFieldMethod, method),
				slog.String(acpFieldSessionID, string(sessionID)),
				slog.String(jsonFieldError, err.Error()),
			)
		}
	})
}

func lifecycleCommandSessionID(method string, params json.RawMessage, result any) (acp.SessionId, bool) {
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

type postResponseHooks struct {
	log *slog.Logger
	mu  sync.Mutex
	all []postResponseHook
}

type postResponseHook struct {
	result json.RawMessage
	run    func()
}

func (h *postResponseHooks) wrap(writer io.Writer) io.Writer {
	return &postResponseWriter{writer: writer, hooks: h}
}

func (h *postResponseHooks) enqueue(result json.RawMessage, run func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.all = append(h.all, postResponseHook{
		result: append(json.RawMessage(nil), result...),
		run:    run,
	})
}

func (h *postResponseHooks) runAfterResponseWrite(data []byte) {
	var msg struct {
		ID     *json.RawMessage `json:"id"`
		Result json.RawMessage  `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &msg); err != nil {
		if h.log != nil {
			h.log.Debug("parse response for post-response hook failed", slog.String(jsonFieldError, err.Error()))
		}

		return
	}

	if msg.ID == nil || len(msg.Result) == 0 || msg.Error != nil {
		return
	}

	h.mu.Lock()
	for index, hook := range h.all {
		if !bytes.Equal(bytes.TrimSpace(hook.result), bytes.TrimSpace(msg.Result)) {
			continue
		}

		h.all = slicesDelete(h.all, index)
		h.mu.Unlock()

		go hook.run()

		return
	}
	h.mu.Unlock()
}

func slicesDelete[S ~[]E, E any](slice S, index int) S {
	return append(slice[:index], slice[index+1:]...)
}

type postResponseWriter struct {
	writer io.Writer
	hooks  *postResponseHooks
}

func (w *postResponseWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err == nil && n == len(data) {
		w.hooks.runAfterResponseWrite(data)
	}

	return n, err
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
			return nil, requestError(err)
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
			return nil, requestError(err)
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

	return c.conn.SendNotification(ctx, acp.ClientMethodElicitationComplete, params)
}

func (c *localAgentConnection) UnstableCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, params, elicitationScope{})
}

func (c *localAgentConnection) CreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
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

	return acp.SendRequest[acp.UnstableCreateElicitationResponse](c.conn, ctx, acp.ClientMethodElicitationCreate, raw)
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

	return acp.SendRequest[acp.WriteTextFileResponse](c.conn, ctx, acp.ClientMethodFsWriteTextFile, params)
}

func (c *localAgentConnection) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	defer release()

	return acp.SendRequest[acp.RequestPermissionResponse](c.conn, ctx, acp.ClientMethodSessionRequestPermission, params)
}

func (c *localAgentConnection) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

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

	return c.conn.SendNotification(ctx, method, params)
}
