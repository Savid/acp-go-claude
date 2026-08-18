package claudeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

type extensionNotification struct {
	method string
	params json.RawMessage
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
}

func newRecordingAgentClient() *recordingAgentClient {
	return &recordingAgentClient{done: make(chan struct{})}
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
) (acp.UnstableCreateElicitationResponse, error) {
	if c.elicitErr != nil {
		return acp.UnstableCreateElicitationResponse{}, c.elicitErr
	}
	if c.elicitResponse != nil {
		c.mu.Lock()
		c.elicitations = append(c.elicitations, request)
		c.mu.Unlock()

		return *c.elicitResponse, nil
	}

	return c.UnstableCreateElicitation(ctx, request)
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
	_ context.Context,
	request acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
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

		return c.recordingClient.SessionUpdate(ctx, notification)
	}
	if c.sessionUpdateErr != nil {
		return c.sessionUpdateErr
	}

	return c.recordingClient.SessionUpdate(ctx, notification)
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

	initialize map[string]any
	settings   map[string]any
	context    map[string]any
	controlErr map[string]error
	queryMsgs  []map[string]any
	onQuery    func()
	sent       []any
	closeCalls int

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
		messages: make(chan map[string]any, 64),
		errs:     make(chan error, 4),
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
	t.mu.Unlock()

	switch msg := payload.(type) {
	case claude.ControlRequest:
		t.respond(msg)
	case map[string]any:
		if msg["type"] == claude.MessageTypeUser {
			for _, queryMsg := range t.queryMsgs {
				t.messages <- queryMsg
			}
			if t.onQuery != nil {
				t.onQuery()
			}
		}
	}

	return nil
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

func (t *fakeClaudeTransport) Messages(context.Context) (<-chan map[string]any, <-chan error) {
	return t.messages, t.errs
}

func (t *fakeClaudeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closeCalls++

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
