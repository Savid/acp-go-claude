package claudeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type extensionNotification struct {
	method string
	params json.RawMessage
}

func interruptCalls(transport *fakeClaudeTransport) int {
	calls := 0
	for _, sent := range transport.Sent() {
		request, ok := sent.(claude.ControlRequest)
		if ok && request.Request["subtype"] == "interrupt" {
			calls++
		}
	}

	return calls
}

type closeHookTransport struct {
	claude.Transport
	onClose func()
}

func (t *closeHookTransport) Close() error {
	if t.onClose != nil {
		t.onClose()
	}

	return t.Transport.Close()
}

type recordingClient struct {
	mu           sync.Mutex
	updates      []acp.SessionNotification
	elicitations []acp.UnstableCreateElicitationRequest
	extensions   []extensionNotification
}

func (c *recordingClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, notification)

	return nil
}

func (c *recordingClient) Updates() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionNotification(nil), c.updates...)
}

func (c *recordingClient) UnstableCreateElicitation(
	_ context.Context,
	request acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitations = append(c.elicitations, request)
	resp := acp.NewUnstableCreateElicitationResponseAccept()
	resp.Accept.Content = map[string]any{"ok": true}

	return resp, nil
}

func (c *recordingClient) Elicitations() []acp.UnstableCreateElicitationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCreateElicitationRequest(nil), c.elicitations...)
}

func (c *recordingClient) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.extensions = append(c.extensions, extensionNotification{method: method, params: append(json.RawMessage(nil), params...)})

	return map[string]any{"ok": true}, nil
}

func (c *recordingClient) Extensions() []extensionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]extensionNotification(nil), c.extensions...)
}

func (*recordingClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (*recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (*recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (*recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (*recordingClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

func (*recordingClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (*recordingClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (*recordingClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

type recordingAgentClient struct {
	recordingClient
	done             chan struct{}
	permission       acp.PermissionOptionId
	permissionErr    error
	nilPermission    bool
	completions      []acp.UnstableCompleteElicitationNotification
	elicitResponse   *acp.UnstableCreateElicitationResponse
	elicitErr        error
	extensionErr     error
	sessionUpdateErr error
	updateCalls      int
	failUpdateAfter  int
	updateSignal     chan struct{}
}

// gatedSessionUpdateClient stops exactly one selected session update at a test
// barrier. Later updates pass through normally, so a close can prove it retained
// the carrier until a post-response producer finished and still complete its
// own terminal publications afterward.
type gatedSessionUpdateClient struct {
	*recordingAgentClient
	gateMu  sync.Mutex
	calls   int
	blockAt int
	entered chan struct{}
	release chan struct{}
}

func newGatedSessionUpdateClient(blockAt int) *gatedSessionUpdateClient {
	return &gatedSessionUpdateClient{
		recordingAgentClient: newRecordingAgentClient(),
		blockAt:              blockAt,
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
}

func (c *gatedSessionUpdateClient) SessionUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
) error {
	c.gateMu.Lock()
	c.calls++
	blocked := c.calls == c.blockAt
	if blocked {
		close(c.entered)
	}
	c.gateMu.Unlock()

	if blocked {
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

func newRecordingAgentClient() *recordingAgentClient {
	return &recordingAgentClient{done: make(chan struct{}), updateSignal: make(chan struct{}, 1)}
}

// admitControlCallbackForTest enters callback code through the same exact
// reservation production installs in the Claude client. Call finish when the
// handler returns so prompt final admission observes the real callback lifetime.
func admitControlCallbackForTest(
	t *testing.T,
	session *agentSession,
	ctx context.Context,
	route string,
) (context.Context, func()) {
	t.Helper()

	admittedCtx, finish, admitted := session.admitControlCallback(ctx, route)
	require.True(t, admitted, "test callback route must have an exact live owner")

	return admittedCtx, finish
}

func handlePermissionThroughAdmissionForTest(
	t *testing.T,
	session *agentSession,
	ctx context.Context,
	route string,
	request claude.PermissionRequest,
) (claude.PermissionDecision, error) {
	t.Helper()

	admittedCtx, finish := admitControlCallbackForTest(t, session, ctx, route)
	defer finish()

	return session.handlePermission(admittedCtx, request)
}

func handleElicitationThroughAdmissionForTest(
	t *testing.T,
	session *agentSession,
	ctx context.Context,
	route string,
	request claude.ElicitationRequest,
) (claude.ElicitationResponse, error) {
	t.Helper()

	admittedCtx, finish := admitControlCallbackForTest(t, session, ctx, route)
	defer finish()

	return session.handleElicitation(admittedCtx, request)
}

func (c *recordingAgentClient) Done() <-chan struct{} {
	if c.done == nil {
		c.done = make(chan struct{})
	}

	return c.done
}

func (c *recordingAgentClient) CreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	_ elicitationScope,
	action actionWireAdmission,
) (acp.UnstableCreateElicitationResponse, error) {
	if err := action.publishPending(); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	c.mu.Lock()
	c.elicitations = append(c.elicitations, request)
	c.mu.Unlock()

	if err := action.observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodElicitationCreate,
		requestID: "test-elicitation-" + action.actionID,
	}); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}
	if c.elicitErr != nil {
		return acp.UnstableCreateElicitationResponse{}, c.elicitErr
	}
	if c.elicitResponse != nil {
		return *c.elicitResponse, nil
	}

	resp := acp.NewUnstableCreateElicitationResponseAccept()
	resp.Accept.Content = map[string]any{"ok": true}

	return resp, nil
}

func (c *recordingAgentClient) UnstableCompleteElicitation(
	_ context.Context,
	notification acp.UnstableCompleteElicitationNotification,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.completions = append(c.completions, notification)

	return nil
}

func (c *recordingAgentClient) Completions() []acp.UnstableCompleteElicitationNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCompleteElicitationNotification(nil), c.completions...)
}

func (c *recordingAgentClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	action actionWireAdmission,
) (acp.RequestPermissionResponse, error) {
	if err := action.publishPending(); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	if err := action.observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodSessionRequestPermission,
		requestID: "test-permission-" + action.actionID,
	}); err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	if c.permissionErr != nil {
		return acp.RequestPermissionResponse{}, c.permissionErr
	}
	if c.nilPermission {
		return acp.RequestPermissionResponse{}, nil
	}
	if c.permission == "" {
		return c.recordingClient.RequestPermission(context.Background(), request)
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(c.permission)}, nil
}

func (c *recordingAgentClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	// The pinned SDK refuses to send a notification whose context is already done,
	// so this double refuses too: a test that delivered an update under a cancelled
	// context would prove nothing about the real transport.
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	c.updateCalls++
	failAfter := c.failUpdateAfter
	calls := c.updateCalls
	c.mu.Unlock()

	if failAfter > 0 {
		if calls >= failAfter {
			return c.sessionUpdateErr
		}

		return c.recordUpdate(ctx, notification)
	}
	if c.sessionUpdateErr != nil {
		return c.sessionUpdateErr
	}

	return c.recordUpdate(ctx, notification)
}

func (c *recordingAgentClient) recordUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
) error {
	if err := c.recordingClient.SessionUpdate(ctx, notification); err != nil {
		return err
	}

	c.mu.Lock()
	if c.updateSignal == nil {
		c.updateSignal = make(chan struct{}, 1)
	}
	signal := c.updateSignal
	c.mu.Unlock()
	select {
	case signal <- struct{}{}:
	default:
	}

	return nil
}

func (c *recordingAgentClient) UpdatesChanged() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.updateSignal == nil {
		c.updateSignal = make(chan struct{}, 1)
	}

	return c.updateSignal
}

func (c *recordingAgentClient) NotifyExtension(_ context.Context, method string, params any) error {
	if c.extensionErr != nil {
		return c.extensionErr
	}

	data, err := json.Marshal(params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.extensions = append(c.extensions, extensionNotification{method: method, params: data})

	return nil
}

type fakeClaudeTransport struct {
	mu sync.Mutex

	startErr error
	closeErr error
	sendErr  error

	initialize  map[string]any
	settings    map[string]any
	context     map[string]any
	controlErr  map[string]error
	queryMsgs   []map[string]any
	queryCount  int
	onQuery     func()
	onSend      func(any)
	sent        []any
	closeCalls  int
	sentSignal  chan struct{}
	closeSignal chan struct{}
	closeOnce   sync.Once

	messages chan map[string]any
	errs     chan error
}

func newFakeClaudeTransport() *fakeClaudeTransport {
	return &fakeClaudeTransport{
		initialize: map[string]any{
			"commands": []any{
				map[string]any{"name": "help", "description": "Help", "argumentHint": "[topic]"},
			},
			"models": []any{
				map[string]any{"value": "sonnet", "displayName": "Sonnet", "description": "balanced", "supportedEffortLevels": []any{"low", "high"}, "supportsAutoMode": true},
				map[string]any{"value": "opus", "displayName": "Opus", "supportedEffortLevels": []any{"high"}},
			},
			"output_style":            "default",
			"available_output_styles": []any{"default", "concise"},
		},
		settings: map[string]any{
			"applied":   map[string]any{"model": "sonnet", "effort": "low"},
			"effective": map[string]any{"fastMode": true},
		},
		context: map[string]any{"totalTokens": 8, "maxTokens": 200000},
		queryMsgs: []map[string]any{
			{
				"type": "stream_event",
				"event": map[string]any{
					"type": "message_start",
					"message": map[string]any{
						"model": "sonnet",
						"usage": map[string]any{"input_tokens": 1, "output_tokens": 2},
					},
				},
			},
			{
				"type": "assistant",
				"uuid": "33333333-3333-4333-8333-333333333333",
				"message": map[string]any{
					"model":       "sonnet",
					"stop_reason": "end_turn",
					"content":     []any{map[string]any{"type": "text", "text": "hello"}},
				},
			},
			{
				"type":        "result",
				"subtype":     "success",
				"is_error":    false,
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 1, "output_tokens": 2},
			},
		},
		messages:    make(chan map[string]any, 64),
		errs:        make(chan error, 4),
		sentSignal:  make(chan struct{}, 1),
		closeSignal: make(chan struct{}),
	}
}

func (t *fakeClaudeTransport) Start(context.Context) error {
	return t.startErr
}

func (t *fakeClaudeTransport) Send(_ context.Context, payload any) error {
	if t.sendErr != nil {
		return t.sendErr
	}

	t.mu.Lock()
	t.sent = append(t.sent, payload)
	if t.sentSignal == nil {
		t.sentSignal = make(chan struct{}, 1)
	}
	sentSignal := t.sentSignal
	t.mu.Unlock()
	select {
	case sentSignal <- struct{}{}:
	default:
	}
	if t.onSend != nil {
		t.onSend(payload)
	}

	switch msg := payload.(type) {
	case claude.ControlRequest:
		t.respond(msg)
	case map[string]any:
		if msg["type"] == claude.MessageTypeUser {
			t.mu.Lock()
			t.queryCount++
			queryCount := t.queryCount
			queryMsgs := append([]map[string]any(nil), t.queryMsgs...)
			t.mu.Unlock()
			for _, queryMsg := range queryMsgs {
				if queryCount > 1 && queryMsg["uuid"] == "33333333-3333-4333-8333-333333333333" {
					cloned := cloneAnyMap(queryMsg)
					cloned["uuid"] = fmt.Sprintf("33333333-3333-4333-8333-%012d", queryCount)
					queryMsg = cloned
				}
				t.messages <- queryMsg
			}
			if t.onQuery != nil {
				t.onQuery()
			}
		}
	}

	return nil
}

func (t *fakeClaudeTransport) SentChanged() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.sentSignal == nil {
		t.sentSignal = make(chan struct{}, 1)
	}

	return t.sentSignal
}

func (t *fakeClaudeTransport) respond(req claude.ControlRequest) {
	subtype, _ := req.Request["subtype"].(string)
	if err := t.controlErr[subtype]; err != nil {
		t.messages <- map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"request_id": req.RequestID,
				"subtype":    "error",
				"error":      err.Error(),
			},
		}

		return
	}

	response := map[string]any{}
	switch subtype {
	case "initialize":
		response = t.initialize
	case "get_settings":
		response = t.settings
	case "get_context_usage":
		response = t.context
	}

	t.messages <- map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"request_id": req.RequestID,
			"subtype":    "success",
			"response":   response,
		},
	}
}

func (t *fakeClaudeTransport) Events(ctx context.Context) <-chan claude.TransportEvent {
	events := make(chan claude.TransportEvent)
	go func() {
		defer close(events)
		messages, errs := t.messages, t.errs
		for messages != nil || errs != nil {
			select {
			case msg, ok := <-messages:
				if !ok {
					messages = nil

					continue
				}
				events <- claude.TransportEvent{Message: msg}
			case err, ok := <-errs:
				if !ok {
					errs = nil

					continue
				}
				events <- claude.TransportEvent{Err: err}

				return
			case <-ctx.Done():
				events <- claude.TransportEvent{Err: ctx.Err()}

				return
			case <-t.closeSignal:
				return
			}
		}
	}()

	return events
}

func (t *fakeClaudeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closeCalls++
	t.closeOnce.Do(func() { close(t.closeSignal) })

	return t.closeErr
}

func (t *fakeClaudeTransport) CloseCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.closeCalls
}

func (t *fakeClaudeTransport) Sent() []any {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]any(nil), t.sent...)
}

func installFakeClaudeClient(agent *Agent, transport *fakeClaudeTransport) {
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, transport)
	}
}
