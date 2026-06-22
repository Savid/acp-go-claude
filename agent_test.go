package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/permissions"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

var testLogger = slog.New(slog.DiscardHandler)

func TestMain(m *testing.M) {
	slog.SetDefault(testLogger)
	goleak.VerifyTestMain(m, goleak.Cleanup(func(int) {
		closeAgentFakeTransportsForTest()
	}))
}

var agentFakeTransports sync.Map

type agentFakeTransport struct {
	incoming  chan map[string]any
	errs      chan error
	closeOnce sync.Once

	mu   sync.Mutex
	sent []any

	assistantText    string
	systemMessages   []map[string]any
	initializeInfo   map[string]any
	controlErrors    map[string]string
	controlResponses map[string]map[string]any
	suppressResult   bool
	resultText       string
	startErr         error
	sendErr          error
	sendHook         func(any)
	closeErr         error
	closed           bool
}

func newAgentFakeTransport() *agentFakeTransport {
	fake := &agentFakeTransport{
		incoming: make(chan map[string]any, 16),
		errs:     make(chan error, 1),
	}
	agentFakeTransports.Store(fake, struct{}{})

	return fake
}

func closeAgentFakeTransportsForTest() {
	agentFakeTransports.Range(func(key, _ any) bool {
		if fake, ok := key.(*agentFakeTransport); ok {
			_ = fake.Close()
		}

		return true
	})
}

func emitResultOnInterrupt(fake *agentFakeTransport, result map[string]any) {
	fake.setSendHook(func(payload any) {
		req, ok := payload.(claude.ControlRequest)
		if !ok {
			return
		}

		subtype, _ := req.Request["subtype"].(string)
		if subtype != "interrupt" {
			return
		}

		fake.incoming <- result
	})
}

func successResultMessage() map[string]any {
	return map[string]any{
		"type":           "result",
		"subtype":        "success",
		"stop_reason":    "end_turn",
		"result":         "",
		"total_cost_usd": 0.01,
	}
}

func (f *agentFakeTransport) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startErr
}

func (f *agentFakeTransport) Send(_ context.Context, payload any) error {
	f.mu.Lock()
	f.sent = append(f.sent, payload)
	err := f.sendErr
	hook := f.sendHook
	assistantText := f.assistantText
	systemMessages := append([]map[string]any(nil), f.systemMessages...)
	initializeInfo := f.initializeInfo
	controlErrors := f.controlErrors
	controlResponses := f.controlResponses
	suppressResult := f.suppressResult
	resultText := f.resultText
	closeErr := f.closeErr
	f.mu.Unlock()

	if hook != nil {
		hook(payload)
	}

	if err != nil {
		return err
	}

	switch typed := payload.(type) {
	case claude.ControlRequest:
		subtype, _ := typed.Request["subtype"].(string)
		if controlErrors != nil && controlErrors[subtype] != "" {
			f.incoming <- map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "error",
					"request_id": typed.RequestID,
					"error":      controlErrors[subtype],
				},
			}

			return nil
		}

		response := map[string]any{}
		if subtype == "initialize" && initializeInfo != nil {
			response = initializeInfo
		}
		if controlResponses != nil && controlResponses[subtype] != nil {
			response = controlResponses[subtype]
		}

		f.incoming <- map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": typed.RequestID,
				"response":   response,
			},
		}
	case map[string]any:
		if typed["type"] == "user" {
			if suppressResult {
				return nil
			}

			if assistantText != "" {
				f.incoming <- map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"content": []any{
							map[string]any{"type": "text", "text": assistantText},
						},
					},
				}
			}

			for _, msg := range systemMessages {
				f.incoming <- msg
			}

			f.incoming <- map[string]any{
				"type":           "result",
				"subtype":        "success",
				"stop_reason":    "end_turn",
				"result":         resultText,
				"total_cost_usd": 0.01,
				"usage": map[string]any{
					"input_tokens":            8,
					"output_tokens":           3,
					"cache_read_input_tokens": 2,
				},
				"modelUsage": map[string]any{
					"claude-test": map[string]any{"contextWindow": 200000},
				},
			}
		}
	}

	return closeErr
}

func (f *agentFakeTransport) Messages(context.Context) (<-chan map[string]any, <-chan error) {
	return f.incoming, f.errs
}

func (f *agentFakeTransport) Close() error {
	f.mu.Lock()
	f.closed = true
	err := f.closeErr
	f.mu.Unlock()

	f.closeOnce.Do(func() {
		select {
		case f.errs <- io.EOF:
		default:
		}
	})

	return err
}

func (f *agentFakeTransport) sentPayloads() []any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]any(nil), f.sent...)
}

func (f *agentFakeTransport) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

func (f *agentFakeTransport) setSendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendErr = err
}

func (f *agentFakeTransport) setSendHook(hook func(any)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendHook = hook
}

func (f *agentFakeTransport) setSuppressResult(suppress bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.suppressResult = suppress
}

func (f *agentFakeTransport) setControlErrors(errors map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.controlErrors = errors
}

type recordingACPClient struct {
	mu sync.Mutex

	updates             []acp.SessionNotification
	permissions         []acp.RequestPermissionRequest
	permission          acp.PermissionOptionId
	permissionCancelled bool
	permissionStarted   chan<- struct{}
	permissionBlock     <-chan struct{}

	elicitations           []acp.UnstableCreateElicitationRequest
	elicitationCompletions []acp.UnstableCompleteElicitationNotification
	elicitationResponse    acp.UnstableCreateElicitationResponse

	permissionErr  error
	elicitationErr error
	updateErr      error
	updateErrAfter int
	extensionErr   error
	extensions     []recordedExtensionNotification
}

var _ acp.Client = (*recordingACPClient)(nil)
var _ acp.ExtensionMethodHandler = (*recordingACPClient)(nil)

type recordedExtensionNotification struct {
	Method string
	Params map[string]any
}

func (c *recordingACPClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{Content: ""}, nil
}

func (c *recordingACPClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (c *recordingACPClient) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, params)
	selected := c.permission
	cancelled := c.permissionCancelled
	permissionErr := c.permissionErr
	started := c.permissionStarted
	block := c.permissionBlock
	c.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return acp.RequestPermissionResponse{}, ctx.Err()
		}
	}

	if selected == "" && len(params.Options) > 0 {
		selected = params.Options[0].OptionId
	}

	if cancelled || selected == "" {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}

	if permissionErr != nil {
		return acp.RequestPermissionResponse{}, permissionErr
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(selected)}, nil
}

func (c *recordingACPClient) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.updateErr != nil {
		if c.updateErrAfter == 0 || len(c.updates) >= c.updateErrAfter {
			return c.updateErr
		}
	}

	c.updates = append(c.updates, params)

	return nil
}

func (c *recordingACPClient) UnstableCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitations = append(c.elicitations, params)
	if c.elicitationErr != nil {
		return acp.UnstableCreateElicitationResponse{}, c.elicitationErr
	}

	if c.elicitationResponse.Accept != nil ||
		c.elicitationResponse.Cancel != nil ||
		c.elicitationResponse.Decline != nil {
		return c.elicitationResponse, nil
	}

	return acp.UnstableCreateElicitationResponse{
		Decline: &acp.UnstableCreateElicitationDecline{Action: claude.ElicitationActionDecline},
	}, nil
}

func (c *recordingACPClient) UnstableCompleteElicitation(
	_ context.Context,
	params acp.UnstableCompleteElicitationNotification,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitationCompletions = append(c.elicitationCompletions, params)

	return nil
}

func (c *recordingACPClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (c *recordingACPClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (c *recordingACPClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (c *recordingACPClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *recordingACPClient) WaitForTerminalExit(
	context.Context,
	acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (c *recordingACPClient) HandleExtensionMethod(
	_ context.Context,
	method string,
	params json.RawMessage,
) (any, error) {
	var decoded map[string]any
	if len(params) > 0 {
		if err := json.Unmarshal(params, &decoded); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.extensionErr != nil {
		return nil, c.extensionErr
	}

	c.extensions = append(c.extensions, recordedExtensionNotification{Method: method, Params: decoded})

	return map[string]any{}, nil
}

func (c *recordingACPClient) recordedUpdates() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionNotification(nil), c.updates...)
}

func (c *recordingACPClient) recordedPermissions() []acp.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.RequestPermissionRequest(nil), c.permissions...)
}

func (c *recordingACPClient) recordedElicitations() []acp.UnstableCreateElicitationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCreateElicitationRequest(nil), c.elicitations...)
}

func (c *recordingACPClient) recordedElicitationCompletions() []acp.UnstableCompleteElicitationNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCompleteElicitationNotification(nil), c.elicitationCompletions...)
}

func (c *recordingACPClient) recordedExtensions() []recordedExtensionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedExtensionNotification(nil), c.extensions...)
}

func (c *recordingACPClient) setPermissionErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.permissionErr = err
}

func (c *recordingACPClient) setElicitationErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitationErr = err
}

func (c *recordingACPClient) setElicitationResponse(resp acp.UnstableCreateElicitationResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitationResponse = resp
}

func connectAgentForTest(t *testing.T, agent *Agent, client acp.Client) *acp.ClientSideConnection {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	clientConn := acp.NewClientSideConnection(client, c2aW, a2cR)
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setConnection(agentConn)

	t.Cleanup(func() {
		_ = agent.Close()
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	return clientConn
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func attachFailingConnection(agent *Agent) {
	agent.setConnection(newLocalAgentConnection(agent, failingWriter{}, strings.NewReader("")))
}

func TestAgentInitialize(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithDefaultModel("claude-test"))
	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{})

	require.NoError(t, err)
	require.Equal(t, acp.ProtocolVersion(acp.ProtocolVersionNumber), resp.ProtocolVersion)
	require.True(t, resp.AgentCapabilities.LoadSession)
	require.NotNil(t, resp.AgentCapabilities.Nes)
	require.NotNil(t, resp.AgentCapabilities.Nes.Events.Document.DidOpen)
	require.Equal(t, acp.TextDocumentSyncKindFull, resp.AgentCapabilities.Nes.Events.Document.DidChange.SyncKind)
	require.NotNil(t, resp.AgentCapabilities.PositionEncoding)
	require.Equal(t, acp.PositionEncodingKindUtf16, *resp.AgentCapabilities.PositionEncoding)
	require.Nil(t, resp.AgentCapabilities.Providers)
	require.True(t, resp.AgentCapabilities.PromptCapabilities.Image)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Close)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Fork)
	require.Equal(t, map[string]any{
		"promptQueueing": map[string]any{
			capabilityScopeKey: capabilityScopeSession,
			"sameSession":      true,
		},
		"sessionImport": map[string]any{
			capabilityScopeKey: capabilityScopeSession,
			"format":           "claude-jsonl",
			"methods": map[string]string{
				"import":       claudeSessionImportMethod,
				"importChunk":  claudeSessionImportChunkMethod,
				"commitImport": claudeSessionCommitImportMethod,
				"abortImport":  claudeSessionAbortImportMethod,
			},
		},
		rawSDKMessagesCapabilityKey: map[string]any{
			capabilityScopeKey:         capabilityScopeSession,
			rawSDKMessagesMethodKey:    rawClaudeSDKMessageMethod,
			rawSDKMessagesEnabledByKey: rawSDKMessagesEnabledByPath,
		},
		outputFormatCapabilityKey: map[string]any{
			capabilityScopeKey:     capabilityScopeSession,
			"types":                []string{ClaudeOutputFormatJSONSchema},
			"config":               outputFormatConfigPath,
			"result":               outputFormatResultPath,
			"hiddenTool":           "StructuredOutput",
			capabilityRawEventsKey: rawClaudeSDKMessageMethod,
		},
		claudeGoalsCapabilityKey: map[string]any{
			capabilityScopeKey:     capabilityScopeSession,
			goalCapabilityStateKey: "session_info_update._meta.claude.goal",
			"initialState": map[string]any{
				"sessionResponses": []string{
					"session/new.result._meta.claude.goal",
					"session/load.result._meta.claude.goal",
					"session/resume.result._meta.claude.goal",
				},
				"listSummary": "session/list.result.sessions[]._meta.claude.goal",
			},
			"setMethod":              claudeSessionSetGoalMethod,
			"semantics":              "full-snapshot",
			"maxObjectiveBytes":      maxGoalObjectiveBytes,
			"maxSummaryRunes":        maxGoalSummaryRunes,
			"statuses":               []string{ClaudeGoalStatusActive, ClaudeGoalStatusCompleted, ClaudeGoalStatusBlocked},
			"clientSettableStatuses": []string{ClaudeGoalStatusActive, ClaudeGoalStatusBlocked},
			"clearValue":             nil,
		},
		"workflows": map[string]any{
			"updates":          true,
			capabilityScopeKey: capabilityScopeSession,
			"toolKind":         "think",
			"metadataPath":     "tool_call_update._meta.claude.workflow",
			"logs": map[string]any{
				"readByDefault": false,
			},
			capabilityRawEventsKey: rawClaudeSDKMessageMethod,
		},
	}, resp.AgentCapabilities.Meta[claudeMetaKey])
	require.Empty(t, resp.AuthMethods)
}

func TestAgentInitializeProtocolVersionNegotiation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested acp.ProtocolVersion
		want      acp.ProtocolVersion
	}{
		{
			name:      "current",
			requested: acp.ProtocolVersionNumber,
			want:      acp.ProtocolVersionNumber,
		},
		{
			name:      "future",
			requested: acp.ProtocolVersion(acp.ProtocolVersionNumber + 1),
			want:      acp.ProtocolVersionNumber,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(WithClaudeHome(t.TempDir()))
			resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: tt.requested})

			require.NoError(t, err)
			require.Equal(t, tt.want, resp.ProtocolVersion)
		})
	}
}

func TestAgentInitializeSelectsClientPositionEncoding(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			PositionEncodings: []acp.PositionEncodingKind{acp.PositionEncodingKindUtf8, acp.PositionEncodingKindUtf32},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp.AgentCapabilities.PositionEncoding)
	require.Equal(t, acp.PositionEncodingKindUtf8, *resp.AgentCapabilities.PositionEncoding)
}

func TestAgentAuthenticate(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	resp, err := agent.Authenticate(context.Background(), acp.AuthenticateRequest{MethodId: "missing"})

	require.Empty(t, resp)
	require.Error(t, err)

	resp, err = agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
	})
	require.Empty(t, resp)
	require.Error(t, err)

	resp, err = agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta:     map[string]any{authMetaGateway: map[string]any{}},
	})
	require.Empty(t, resp)
	require.Error(t, err)
}

func TestGatewayAuthMetaValidation(t *testing.T) {
	t.Parallel()

	auth, err := parseGatewayAuthMeta(map[string]any{
		authMetaGateway: map[string]any{
			gatewayBaseURLKey: " https://gateway.example/path ",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://gateway.example/path", auth.BaseURL)

	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "missing base url",
			meta: map[string]any{authMetaGateway: map[string]any{}},
			want: "baseUrl is required",
		},
		{
			name: "empty base url",
			meta: map[string]any{authMetaGateway: map[string]any{gatewayBaseURLKey: " "}},
			want: "baseUrl is required",
		},
		{
			name: "alternate baseURL casing",
			meta: map[string]any{authMetaGateway: map[string]any{"baseURL": "https://gateway.example"}},
			want: "baseUrl is required",
		},
		{
			name: "title BaseUrl casing",
			meta: map[string]any{authMetaGateway: map[string]any{"BaseUrl": "https://gateway.example"}},
			want: "baseUrl is required",
		},
		{
			name: "upper BaseURL casing",
			meta: map[string]any{authMetaGateway: map[string]any{"BaseURL": "https://gateway.example"}},
			want: "baseUrl is required",
		},
		{
			name: "relative base url",
			meta: map[string]any{authMetaGateway: map[string]any{gatewayBaseURLKey: "/gateway"}},
			want: "absolute HTTPS URL",
		},
		{
			name: "http base url",
			meta: map[string]any{authMetaGateway: map[string]any{gatewayBaseURLKey: "http://gateway.example"}},
			want: "must use HTTPS",
		},
		{
			name: "invalid header name",
			meta: map[string]any{
				authMetaGateway: map[string]any{
					gatewayBaseURLKey: "https://gateway.example",
					"headers":         map[string]any{"bad header": "value"},
				},
			},
			want: "invalid name",
		},
		{
			name: "empty header name",
			meta: map[string]any{
				authMetaGateway: map[string]any{
					gatewayBaseURLKey: "https://gateway.example",
					"headers":         map[string]any{"": "value"},
				},
			},
			want: "invalid name",
		},
		{
			name: "invalid header value",
			meta: map[string]any{
				authMetaGateway: map[string]any{
					gatewayBaseURLKey: "https://gateway.example",
					"headers":         map[string]any{"x-api-key": "good\nbad"},
				},
			},
			want: "invalid value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseGatewayAuthMeta(tc.meta)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAgentInitializeAuthMethods(t *testing.T) {
	t.Run("terminal auth methods use claude passthrough args", func(t *testing.T) {
		agent := NewAgent(WithClaudePath("/bin/claude"), WithClaudeHome("/tmp/claude-home"))
		resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
			ClientCapabilities: acp.ClientCapabilities{
				Auth: acp.AuthCapabilities{Terminal: true},
			},
		})

		require.NoError(t, err)
		claudeLogin := requireAuthMethodTerminal(t, resp.AuthMethods, authMethodClaudeAI)
		require.Equal(t, []string{
			terminalAuthCLIMarker,
			terminalAuthClaudeFlag,
			"/bin/claude",
			terminalAuthHomeFlag,
			"/tmp/claude-home",
			"auth",
			"login",
			"--claudeai",
		}, claudeLogin.Args)
		require.Nil(t, claudeLogin.Meta)

		consoleLogin := requireAuthMethodTerminal(t, resp.AuthMethods, authMethodConsole)
		require.Equal(t, []string{
			terminalAuthCLIMarker,
			terminalAuthClaudeFlag,
			"/bin/claude",
			terminalAuthHomeFlag,
			"/tmp/claude-home",
			"auth",
			"login",
			"--console",
		}, consoleLogin.Args)
	})

	t.Run("meta terminal auth and gateway", func(t *testing.T) {
		agent := NewAgent(WithHideClaudeAuth(true))
		resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
			ClientCapabilities: acp.ClientCapabilities{
				Meta: map[string]any{authMetaTerminalAuth: true},
				Auth: acp.AuthCapabilities{
					Meta: map[string]any{authMetaGateway: true},
				},
			},
		})

		require.NoError(t, err)
		require.Nil(t, findAuthMethodTerminal(resp.AuthMethods, authMethodClaudeAI))

		consoleLogin := requireAuthMethodTerminal(t, resp.AuthMethods, authMethodConsole)
		require.Equal(t, []string{terminalAuthCLIMarker, "auth", "login", "--console"}, consoleLogin.Args)
		require.Contains(t, consoleLogin.Meta, authMetaTerminalAuth)

		gateway := requireAuthMethodAgent(t, resp.AuthMethods, authMethodGateway)
		require.Equal(t, "Custom model gateway", gateway.Name)
		require.Equal(t, map[string]any{"protocol": authGatewayProtocol}, gateway.Meta[authMetaGateway])
	})

	t.Run("remote terminal auth uses current login commands", func(t *testing.T) {
		t.Setenv("SSH_TTY", "/dev/pts/1")

		agent := NewAgent()
		resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
			ClientCapabilities: acp.ClientCapabilities{
				Auth: acp.AuthCapabilities{Terminal: true},
			},
		})

		require.NoError(t, err)
		claudeLogin := requireAuthMethodTerminal(t, resp.AuthMethods, authMethodClaudeAI)
		require.Equal(t, []string{terminalAuthCLIMarker, "auth", "login", "--claudeai"}, claudeLogin.Args)
		consoleLogin := requireAuthMethodTerminal(t, resp.AuthMethods, authMethodConsole)
		require.Equal(t, []string{terminalAuthCLIMarker, "auth", "login", "--console"}, consoleLogin.Args)
	})

	t.Run("hide claude auth keeps console login in remote environments", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_REMOTE", "1")

		agent := NewAgent(WithHideClaudeAuth(true))
		resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
			ClientCapabilities: acp.ClientCapabilities{
				Auth: acp.AuthCapabilities{Terminal: true},
			},
		})

		require.NoError(t, err)
		require.Nil(t, findAuthMethodTerminal(resp.AuthMethods, authMethodClaudeAI))
		require.NotNil(t, findAuthMethodTerminal(resp.AuthMethods, authMethodConsole))
	})
}

func TestAgentHandleExtensionMethod(t *testing.T) {
	t.Parallel()

	resp, err := NewAgent().HandleExtensionMethod(context.Background(), "_unknown", nil)

	require.Nil(t, resp)

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32601, reqErr.Code)
	require.Equal(t, map[string]any{"method": "_unknown"}, reqErr.Data)
}

func TestServeReturnsContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input, inputWriter := io.Pipe()
	outputReader, output := io.Pipe()
	t.Cleanup(func() {
		_ = input.Close()
		_ = inputWriter.Close()
		_ = output.Close()
		_ = outputReader.Close()
	})

	err := Serve(ctx, input, output, WithClaudeHome(t.TempDir()))
	require.ErrorIs(t, err, context.Canceled)
}

func TestServeLogsCloseErrors(t *testing.T) {
	previousNewServeAgent := newServeAgent
	t.Cleanup(func() { newServeAgent = previousNewServeAgent })

	newServeAgent = func(opts ...Option) *Agent {
		agent := NewAgent(opts...)
		fake := newAgentFakeTransport()
		fake.closeErr = errors.New("close failed")
		agent.sessions["session-1"] = &Session{
			id:     "session-1",
			client: claude.NewClient(nil, claude.Options{}, fake),
		}

		return agent
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input, inputWriter := io.Pipe()
	outputReader, output := io.Pipe()
	t.Cleanup(func() {
		_ = input.Close()
		_ = inputWriter.Close()
		_ = output.Close()
		_ = outputReader.Close()
	})

	err := Serve(ctx, input, output, WithClaudeHome(t.TempDir()))
	require.ErrorIs(t, err, context.Canceled)
}

func TestServeReturnsWhenConnectionStops(t *testing.T) {
	t.Parallel()

	err := Serve(context.Background(), strings.NewReader(""), io.Discard, WithClaudeHome(t.TempDir()))
	require.NoError(t, err)
}

func TestAgentCloseClosesSessionsAndState(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:                   "/repo",
		AdditionalDirectories: []string{"/shared"},
		McpServers:            []acp.McpServer{},
	})
	require.NoError(t, err)

	agent.documents[resp.SessionId] = map[string]documentState{}
	agent.focusedDocuments[resp.SessionId] = "file:///repo/main.go"
	agent.nesSessions[resp.SessionId] = &nesSession{}

	require.NoError(t, agent.Close())
	require.True(t, fake.isClosed())

	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
	require.Empty(t, agent.documents)
	require.Empty(t, agent.focusedDocuments)
	require.Empty(t, agent.nesSessions)
}

func TestAgentCloseReportsSessionErrorsAndClosesSessionMCPBridge(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	fake := newAgentFakeTransport()
	fake.closeErr = closeErr
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.sessions["session-1"] = &Session{
		id:     "session-1",
		client: claude.NewClient(nil, claude.Options{}, fake),
	}
	conn := &mcpBridgeConn{
		conn:    left,
		closed:  make(chan struct{}),
		pending: map[string]chan mcpRPCMessage{"1": make(chan mcpRPCMessage)},
	}
	bridge := &mcpSessionBridge{
		agent: agent,
		done:  make(chan struct{}),
		conns: map[*mcpBridgeConn]struct{}{conn: {}},
	}
	agent.sessions["session-1"].mcpBridge = bridge

	err := agent.Close()
	require.ErrorIs(t, err, closeErr)
	require.True(t, fake.isClosed())

	select {
	case <-conn.closed:
	default:
		t.Fatal("MCP connection was not closed")
	}
}

func TestAgentRemoveSessionLogsCloseErrors(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.closeErr = errors.New("close failed")
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	session := &Session{
		id:     "session-1",
		client: claude.NewClient(nil, claude.Options{}, fake),
	}
	agent.sessions[session.id] = session

	agent.removeSession(context.Background(), session.id, session)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)
	agent.removeSession(context.Background(), "other", nil)
}

func TestAgentDefaultClaudeClientFactory(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	require.NotNil(t, agent.newClaudeClient(nil, claude.Options{}))
}

func TestAgentNewSessionUUIDError(t *testing.T) {
	random := uuidRandom
	uuidRandom = errReader{err: errors.New("random failed")}
	t.Cleanup(func() {
		uuidRandom = random
	})

	_, err := NewAgent().NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo"})
	require.Error(t, err)
}

func TestAgentNewSessionRejectsInvalidClaudeHome(t *testing.T) {
	t.Parallel()

	claudeHome := filepath.Join(t.TempDir(), "claude-home")
	require.NoError(t, os.WriteFile(claudeHome, []byte("not a directory"), 0o600))

	_, err := NewAgent(WithClaudeHome(claudeHome)).NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func TestAgentNewSessionAndPrompt(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, "/repo", options.Cwd)
		require.Equal(t, "claude-test", options.Model)

		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.SessionId)
	modelConfig := findSelectConfig(resp.ConfigOptions, configModel)
	require.NotNil(t, modelConfig)
	require.Equal(t, acp.SessionConfigValueId("claude-test"), modelConfig.CurrentValue)
	modelOption := findSelectOption(*modelConfig.Options.Ungrouped, "claude-test")
	require.NotNil(t, modelOption)
	require.Equal(t, "claude-test", modelOption.Name)

	messageID := "22222222-2222-4222-8222-222222222222"
	promptResp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: resp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		MessageId: &messageID,
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, promptResp.StopReason)
	require.Equal(t, &messageID, promptResp.UserMessageId)
	require.NotNil(t, promptResp.Usage)
	require.Equal(t, 13, promptResp.Usage.TotalTokens)
}

func TestAgentUsesClaudeInitializeMetadata(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"commands": []any{
			map[string]any{
				"name":         "debug",
				"description":  "Enable debug logging",
				"argumentHint": "[issue]",
			},
		},
		"models": []any{
			map[string]any{
				"value":       "default",
				"displayName": "Default (recommended)",
				"description": "Use Claude Code's default model",
			},
			map[string]any{
				"value":       "sonnet",
				"displayName": "Sonnet",
				"description": "Best for everyday tasks",
			},
		},
		"output_style":            "default",
		"available_output_styles": []any{"default", "Explanatory"},
	}

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	requireNoTopLevelConfigState(t, session)
	require.Equal(t, acp.SessionConfigValueId("default"), session.ConfigOptions[0].Select.CurrentValue)
	require.Equal(t, acp.SessionConfigOptionCategoryModel, *session.ConfigOptions[0].Select.Category)
	require.Equal(t, "Sonnet", (*session.ConfigOptions[0].Select.Options.Ungrouped)[1].Name)
	require.Equal(t, "Default (recommended)", (*session.ConfigOptions[0].Select.Options.Ungrouped)[0].Name)
	require.Equal(t, "Use Claude Code's default model", *(*session.ConfigOptions[0].Select.Options.Ungrouped)[0].Description)
	require.Len(t, session.ConfigOptions, 4)
	require.Equal(t, configMode, session.ConfigOptions[1].Select.Id)
	require.Equal(t, acp.SessionConfigOptionCategoryMode, *session.ConfigOptions[1].Select.Category)
	require.Equal(t, configOutputStyle, session.ConfigOptions[2].Select.Id)
	require.Equal(t, acp.SessionConfigValueId("default"), session.ConfigOptions[2].Select.CurrentValue)
	require.Len(t, *session.ConfigOptions[2].Select.Options.Ungrouped, 2)
	require.Equal(t, configFastMode, session.ConfigOptions[3].Boolean.Id)
	require.Equal(t, modelConfigCategory, *session.ConfigOptions[3].Boolean.Category)
	require.False(t, session.ConfigOptions[3].Boolean.CurrentValue)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 1
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	commands := updates[0].Update.AvailableCommandsUpdate.AvailableCommands
	require.Len(t, commands, 1)
	require.Equal(t, "debug", commands[0].Name)
	require.Equal(t, "Enable debug logging", commands[0].Description)
	require.Equal(t, "[issue]", commands[0].Input.Unstructured.Hint)
}

func TestAgentUsesClaudeSettingsMetadata(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.controlResponses = map[string]map[string]any{
		"get_settings": {
			"applied": map[string]any{
				"model":  "claude-settings",
				"effort": "low",
			},
			"effective": map[string]any{
				"fastMode": true,
			},
		},
	}

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	modelConfig := findSelectConfig(session.ConfigOptions, configModel)
	require.NotNil(t, modelConfig)
	require.Equal(t, acp.SessionConfigValueId("claude-settings"), modelConfig.CurrentValue)
	require.Empty(t, sentControlRequests(fake, "set_model"))

	fastConfig := findBooleanConfig(session.ConfigOptions, configFastMode)
	require.NotNil(t, fastConfig)
	require.True(t, fastConfig.CurrentValue)
}

func TestAgentUsesClaudeSettingsFiles(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, settingsFileName), []byte(`{
		"model": "opus",
		"effortLevel": "high",
		"availableModels": ["opus", "custom"],
		"permissions": {"defaultMode": "auto"},
		"env": {"FROM_SETTINGS": "yes"}
	}`), 0o600))

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{"value": "default", "displayName": "Default"},
			map[string]any{
				"value":                 "claude-opus-4-6",
				"displayName":           "Opus",
				"supportsAutoMode":      true,
				"supportsEffort":        true,
				"supportedEffortLevels": []any{"low", "high"},
			},
		},
	}

	agent := NewAgent(WithClaudeHome(home))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, "auto", options.PermissionMode)
		require.Equal(t, "yes", options.Env["FROM_SETTINGS"])

		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	requireClaudeVariantMeta(t, session.Meta, "opus", "high", []string{"low", "high"})
	modeConfig := findSelectConfig(session.ConfigOptions, configMode)
	require.NotNil(t, modeConfig)
	require.Equal(t, acp.SessionConfigValueId(modeAuto), modeConfig.CurrentValue)
	modelConfig := findSelectConfig(session.ConfigOptions, configModel)
	require.NotNil(t, modelConfig)
	require.Equal(t, acp.SessionConfigValueId("opus"), modelConfig.CurrentValue)
	requireModelOptionBasics(t, *modelConfig.Options.Ungrouped, []acp.SessionConfigSelectOption{
		{Name: "Default", Value: "default"},
		{
			Name:  "Opus",
			Value: "opus",
			Meta: claudeModelMetaForTest(map[string]any{
				claudeModelMetaContextWindowKey:    largeContextWindow,
				claudeModelMetaSupportedEffortKey:  []string{"low", "high"},
				claudeModelMetaSupportsAutoModeKey: true,
			}),
		},
		{Name: "custom", Value: "custom"},
	})

	effortConfig := findSelectConfig(session.ConfigOptions, configEffort)
	require.NotNil(t, effortConfig)
	require.Equal(t, acp.SessionConfigValueId("high"), effortConfig.CurrentValue)
}

func TestAgentPassesSettingSourcesToClaude(t *testing.T) {
	t.Parallel()

	var captured []claude.Options
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = append(captured, options)

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.Equal(t, []string{"user", "project", "local"}, captured[0].SettingSources)

	captured = nil
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSettingSources())
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = append(captured, options)

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	_, err = agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.Empty(t, captured[0].SettingSources)
	require.NotNil(t, captured[0].SettingSources)
}

func TestAgentUsesTypedSessionMetaOptions(t *testing.T) {
	t.Parallel()

	var captured []claude.Options
	agent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithDefaultSystemPrompt("global system"),
		WithEnv(map[string]string{"GLOBAL": "1", "OVERRIDE": "global"}),
	)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = append(captured, options)

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:                   "/repo",
		AdditionalDirectories: []string{"/shared"},
		McpServers:            []acp.McpServer{},
		Meta: map[string]any{
			claudeMetaKey: map[string]any{
				metaOptionsKey: map[string]any{
					metaBareKey: true,
					settingsFieldEnv: map[string]any{
						"SESSION":  "1",
						"OVERRIDE": "session",
					},
					metaSystemPromptKey:          "session system",
					metaModelKey:                 "opus",
					metaPermissionModeKey:        permissionModeDontAsk,
					metaAdditionalDirectoriesKey: []any{"/options-root"},
					metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey: ClaudeOutputFormatJSONSchema,
						metaOutputFormatSchemaKey: map[string]any{
							"type": "object",
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, captured, 1)
	require.Equal(t, "session system", captured[0].SystemText)
	require.True(t, captured[0].Bare)
	require.Equal(t, map[string]string{
		"GLOBAL":   "1",
		"SESSION":  "1",
		"OVERRIDE": "session",
	}, captured[0].Env)
	require.Equal(t, "opus", captured[0].Model)
	require.Equal(t, permissionModeDontAsk, captured[0].PermissionMode)
	require.Equal(t, []string{"/shared", "/options-root"}, captured[0].AddDirs)
	require.Equal(t, map[string]any{"type": "object"}, captured[0].JSONSchema)

	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, resp.SessionId, list.Sessions[0].SessionId)
	require.Equal(t, []string{"/shared", "/options-root"}, list.Sessions[0].AdditionalDirectories)
}

func TestAgentIgnoresTopLevelSessionMetaSystemPrompt(t *testing.T) {
	t.Parallel()

	var captured claude.Options
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithDefaultSystemPrompt("global system"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
		Meta: map[string]any{
			metaSystemPromptKey: "top-level system",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "global system", captured.SystemText)
}

func TestAgentRejectsUnsupportedSessionMetaOptions(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, _ claude.Options) *claude.Client {
		t.Fatal("Claude client should not start for invalid session meta")

		return nil
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
		Meta: map[string]any{
			claudeMetaKey: map[string]any{
				metaOptionsKey: map[string]any{"cwd": "/other"},
			},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "_meta.claude.options.cwd is not supported")
}

func TestAgentSessionPathValidation(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, _ claude.Options) *claude.Client {
		t.Fatal("Claude client should not start for invalid session paths")

		return nil
	}

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "new relative cwd",
			call: func() error {
				_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
					Cwd:        "repo",
					McpServers: []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "new relative additional directory",
			call: func() error {
				_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
					Cwd:                   "/repo",
					AdditionalDirectories: []string{"relative"},
					McpServers:            []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "new relative meta additional directory",
			call: func() error {
				_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
					Cwd:        "/repo",
					McpServers: []acp.McpServer{},
					Meta: map[string]any{
						claudeMetaKey: map[string]any{
							metaOptionsKey: map[string]any{
								metaAdditionalDirectoriesKey: []any{"relative"},
							},
						},
					},
				})

				return err
			},
		},
		{
			name: "resume relative cwd",
			call: func() error {
				_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
					SessionId:  "session-1",
					Cwd:        "repo",
					McpServers: []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "resume relative additional directory",
			call: func() error {
				_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
					SessionId:             "session-1",
					Cwd:                   "/repo",
					AdditionalDirectories: []string{"relative"},
					McpServers:            []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "load relative cwd",
			call: func() error {
				_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
					SessionId:  "session-1",
					Cwd:        "repo",
					McpServers: []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "load relative additional directory",
			call: func() error {
				_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
					SessionId:             "session-1",
					Cwd:                   "/repo",
					AdditionalDirectories: []string{"relative"},
					McpServers:            []acp.McpServer{},
				})

				return err
			},
		},
		{
			name: "fork relative cwd",
			call: func() error {
				_, err := agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
					SessionId:  "session-1",
					Cwd:        "repo",
					McpServers: []acp.UnstableMcpServer{},
				})

				return err
			},
		},
		{
			name: "fork relative additional directory",
			call: func() error {
				_, err := agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
					SessionId:             "session-1",
					Cwd:                   "/repo",
					AdditionalDirectories: []string{"relative"},
					McpServers:            []acp.UnstableMcpServer{},
				})

				return err
			},
		},
		{
			name: "list relative cwd",
			call: func() error {
				cwd := "repo"
				_, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})

				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.call()
			require.Error(t, err)

			var reqErr *acp.RequestError
			require.ErrorAs(t, err, &reqErr)
			require.Equal(t, -32602, reqErr.Code)
		})
	}
}

func TestAgentModelConfigEnvAndAliases(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":       "default",
				"displayName": "Default",
			},
			map[string]any{
				"value":                 "claude-opus-4-6",
				"displayName":           "Opus",
				"description":           "Large model",
				"supportsAutoMode":      true,
				"supportsEffort":        true,
				"supportedEffortLevels": []any{"low", "high"},
			},
			map[string]any{
				"value":       "claude-sonnet-4-5",
				"displayName": "Sonnet",
			},
		},
	}
	agent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithEnv(map[string]string{
			envAnthropicModel: "opus",
			envClaudeModelConfig: `{
				"modelOverrides": {"opus": "provider-opus"},
				"availableModels": ["opus", "custom"]
			}`,
		}),
	)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	requireClaudeVariantMeta(t, session.Meta, "opus", "", []string{"low", "high"})
	modelConfig := findSelectConfig(session.ConfigOptions, configModel)
	require.NotNil(t, modelConfig)
	require.Equal(t, acp.SessionConfigValueId("opus"), modelConfig.CurrentValue)
	requireModelOptionBasics(t, *modelConfig.Options.Ungrouped, []acp.SessionConfigSelectOption{
		{Name: "Default", Value: "default"},
		{
			Name:        "Opus",
			Value:       "opus",
			Description: stringPtrIfNotEmpty("Large model"),
			Meta: claudeModelMetaForTest(map[string]any{
				claudeModelMetaContextWindowKey:    largeContextWindow,
				claudeModelMetaSupportedEffortKey:  []string{"low", "high"},
				claudeModelMetaSupportsAutoModeKey: true,
			}),
		},
		{Name: "custom", Value: "custom"},
	})
	modeConfig := findSelectConfig(session.ConfigOptions, configMode)
	require.NotNil(t, modeConfig)
	require.NotNil(t, findSelectOption(*modeConfig.Options.Ungrouped, acp.SessionConfigValueId(modeAuto)))

	requests := sentControlRequests(fake, "set_model")
	require.NotEmpty(t, requests)
	require.Equal(t, "provider-opus", requests[len(requests)-1].Request["model"])
}

func TestAgentModelAliasAndModeGating(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":            "claude-opus-4-6",
				"displayName":      "Opus",
				"supportsAutoMode": true,
			},
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	agent := NewAgent(WithEnv(map[string]string{envAnthropicModel: "opus"}))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	modeConfig := findSelectConfig(session.ConfigOptions, configMode)
	require.NotNil(t, modeConfig)
	require.Equal(t, acp.SessionConfigValueId(modeDefault), modeConfig.CurrentValue)
	require.NotNil(t, findSelectOption(*modeConfig.Options.Ungrouped, acp.SessionConfigValueId(modeAuto)))

	_, err = setModeConfig(context.Background(), agent, session.SessionId, modeAuto)
	require.NoError(t, err)

	configResp, err := agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  configModel,
			Value:     "haiku",
		},
	})
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId("claude-haiku-4-5"), configResp.ConfigOptions[0].Select.CurrentValue)

	_, err = setModeConfig(context.Background(), agent, session.SessionId, modeAuto)
	require.Error(t, err)

	requests := sentControlRequests(fake, "set_model")
	require.Equal(t, "claude-haiku-4-5", requests[len(requests)-1].Request["model"])

	permissionRequests := sentControlRequests(fake, "set_permission_mode")
	require.Equal(t, "default", permissionRequests[len(permissionRequests)-1].Request["mode"])
}

func TestAgentModelSwitchReconcilesEffort(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":                 "opus",
				"displayName":           "Opus",
				"supportsEffort":        true,
				"supportedEffortLevels": []any{"low", "xhigh"},
			},
			map[string]any{
				"value":                 "sonnet",
				"displayName":           "Sonnet",
				"supportsEffort":        true,
				"supportedEffortLevels": []any{"low", "high"},
			},
			map[string]any{
				"value":       "haiku",
				"displayName": "Haiku",
			},
		},
	}
	fake.controlResponses = map[string]map[string]any{
		"get_settings": {
			"applied": map[string]any{"effort": "xhigh"},
		},
	}

	agent := NewAgent(WithDefaultModel("opus"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionResp.SessionId,
			ConfigId:  configModel,
			Value:     "sonnet",
		},
	})
	require.NoError(t, err)

	_, err = setModelConfig(context.Background(), agent, sessionResp.SessionId, "haiku")
	require.NoError(t, err)

	requests := sentControlRequests(fake, "apply_flag_settings")
	require.NotEmpty(t, requests)
	require.Equal(t, map[string]any{"effort": "high"}, requests[len(requests)-2].Request["settings"])
	require.Equal(t, map[string]any{"effort": nil}, requests[len(requests)-1].Request["settings"])

	session, err := agent.session(sessionResp.SessionId)
	require.NoError(t, err)
	require.Empty(t, session.effort)
}

func TestSelectInitialModel(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{Value: "claude-opus-4-6", DisplayName: "Opus"},
		{Value: "claude-sonnet-4-5", DisplayName: "Sonnet"},
	}

	for _, tc := range []struct {
		name          string
		defaultModel  string
		envModel      string
		settingsModel string
		available     []claude.AvailableModelInfo
		want          initialModelSelection
	}{
		{
			name:         "default alias resolves and applies when startup default differs",
			defaultModel: "opus",
			available:    available,
			want:         initialModelSelection{Model: "claude-opus-4-6", ShouldApply: true},
		},
		{
			name:         "default exact already applied by process args",
			defaultModel: "claude-opus-4-6",
			available:    available,
			want:         initialModelSelection{Model: "claude-opus-4-6"},
		},
		{
			name:      "env model applies",
			envModel:  "sonnet",
			available: available,
			want:      initialModelSelection{Model: "claude-sonnet-4-5", ShouldApply: true},
		},
		{
			name:          "settings model is recorded without reapplying",
			settingsModel: "claude-opus-4-6",
			available:     available,
			want:          initialModelSelection{Model: "claude-opus-4-6"},
		},
		{
			name:      "available fallback applies",
			available: available,
			want:      initialModelSelection{Model: "claude-opus-4-6", ShouldApply: true},
		},
		{
			name: "empty",
			want: initialModelSelection{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, selectInitialModel(
				tc.defaultModel,
				tc.envModel,
				tc.settingsModel,
				tc.available,
			))
		})
	}
}

func TestAgentModelModeClampPermissionError(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":            "claude-opus-4-6",
				"displayName":      "Opus",
				"supportsAutoMode": true,
			},
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	agent := NewAgent(WithEnv(map[string]string{envAnthropicModel: "opus"}))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = setModeConfig(context.Background(), agent, session.SessionId, modeAuto)
	require.NoError(t, err)

	fake.controlErrors = map[string]string{"set_permission_mode": "mode failed"}
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  configModel,
			Value:     "haiku",
		},
	})
	require.Error(t, err)
}

func TestAgentInvalidModelConfig(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithEnv(map[string]string{envClaudeModelConfig: "not-json"}))

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.Error(t, err)

	bridgeAgent := NewAgent(
		WithMCPProxyCommand("proxy"),
		WithEnv(map[string]string{envClaudeModelConfig: "not-json"}),
	)
	bridgeAgent.setConnection(&stubAgentClient{})

	_, err = bridgeAgent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)
}

func TestAgentInitialModelSetErrorClosesMCPBridge(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":       "claude-opus-4-6",
				"displayName": "Opus",
			},
		},
	}
	fake.controlErrors = map[string]string{"set_model": "model failed"}

	agent := NewAgent(
		WithMCPProxyCommand("proxy"),
		WithEnv(map[string]string{envAnthropicModel: "opus"}),
	)
	agent.setConnection(&stubAgentClient{})
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
}

func TestAgentInitialPermissionModeClamp(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	agent := NewAgent(WithDefaultPermissionMode(string(modeAuto)))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	modeConfig := findSelectConfig(session.ConfigOptions, configMode)
	require.NotNil(t, modeConfig)
	require.Equal(t, acp.SessionConfigValueId(modeDefault), modeConfig.CurrentValue)

	requests := sentControlRequests(fake, "set_permission_mode")
	require.NotEmpty(t, requests)
	require.Equal(t, "default", requests[len(requests)-1].Request["mode"])
}

func TestAgentInitialPermissionModeClampErrorClosesMCPBridge(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	fake.controlErrors = map[string]string{"set_permission_mode": "mode failed"}

	agent := NewAgent(
		WithDefaultPermissionMode(string(modeAuto)),
		WithMCPProxyCommand("proxy"),
	)
	agent.setConnection(&stubAgentClient{})
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
}

func TestAgentSetSessionModelConfigModeClamp(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":            "claude-opus-4-6",
				"displayName":      "Opus",
				"supportsAutoMode": true,
			},
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	agent := NewAgent(WithEnv(map[string]string{envAnthropicModel: "opus"}))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = setModeConfig(context.Background(), agent, session.SessionId, modeAuto)
	require.NoError(t, err)

	modelResp, err := setModelConfig(context.Background(), agent, session.SessionId, "haiku")
	require.NoError(t, err)
	modelConfig := findSelectConfig(modelResp.ConfigOptions, configModel)
	require.NotNil(t, modelConfig)
	require.Equal(t, acp.SessionConfigValueId("claude-haiku-4-5"), modelConfig.CurrentValue)
}

func TestAgentSetSessionModelConfigModeClampPermissionError(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":            "claude-opus-4-6",
				"displayName":      "Opus",
				"supportsAutoMode": true,
			},
			map[string]any{
				"value":       "claude-haiku-4-5",
				"displayName": "Haiku",
			},
		},
	}
	agent := NewAgent(WithEnv(map[string]string{envAnthropicModel: "opus"}))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = setModeConfig(context.Background(), agent, session.SessionId, modeAuto)
	require.NoError(t, err)

	fake.controlErrors = map[string]string{"set_permission_mode": "mode failed"}
	_, err = setModelConfig(context.Background(), agent, session.SessionId, "haiku")
	require.Error(t, err)
}

func TestModePermissionMappingAndAvailability(t *testing.T) {
	t.Parallel()

	require.Equal(t, modePlan, acpModeForPermission(string(modePlan)))
	require.Equal(t, modeAcceptEdits, acpModeForPermission("acceptEdits"))
	require.Equal(t, modeBypassPermissions, acpModeForPermission("bypassPermissions"))
	require.Equal(t, modeAuto, acpModeForPermission(string(modeAuto)))
	require.Equal(t, modeDontAsk, acpModeForPermission("dontAsk"))
	require.Equal(t, modeDefault, acpModeForPermission("unknown"))
	require.False(t, modeAvailableForModel("unknown", "", nil))
}

func TestAgentSessionStartHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, "http", mcpServerName(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http"}}))
	require.Equal(t, "sse", mcpServerName(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}))
	require.Equal(t, "acp", mcpServerName(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}))
	require.Equal(t, "stdio", mcpServerName(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}))
	require.Empty(t, mcpServerName(acp.McpServer{}))
	require.False(t, missingClaudeSessionError(nil))

	require.NotEmpty(t, sessionStartFingerprint(sessionStart{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Stdio: &acp.McpServerStdio{Name: "b"}},
			{Http: &acp.McpServerHttpInline{Name: "a"}},
		},
	}))
	require.NotEqual(t,
		sessionStartFingerprint(sessionStart{Cwd: "/repo", MetaOptions: ClaudeOptions{
			OutputFormat: &ClaudeOutputFormat{Type: ClaudeOutputFormatJSONSchema, Schema: map[string]any{"title": "first"}},
		}}),
		sessionStartFingerprint(sessionStart{Cwd: "/repo", MetaOptions: ClaudeOptions{
			OutputFormat: &ClaudeOutputFormat{Type: ClaudeOutputFormatJSONSchema, Schema: map[string]any{"title": "second"}},
		}}),
	)
	require.Contains(t, jsonFingerprint(func() {}), "marshal-error:")
}

func TestAgentWarnsDeprecatedSSEMCPServers(t *testing.T) {
	t.Parallel()

	names := sseMCPServerNames([]acp.McpServer{
		{Http: &acp.McpServerHttpInline{Name: "http"}},
		{Sse: &acp.McpServerSseInline{Name: "sse"}},
	})
	require.Equal(t, []string{"sse"}, names)

	var logs bytes.Buffer
	agent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{}))))
	agent.warnDeprecatedSSEMCPServers(context.Background(), "test", nil)
	agent.warnDeprecatedSSEMCPServers(context.Background(), "test", names)

	require.Contains(t, logs.String(), "SSE MCP transport is deprecated")
	require.Contains(t, logs.String(), "sse")
	require.Contains(t, logs.String(), "test")
}

func TestBypassPermissionAvailability(t *testing.T) {
	previousGeteuid := osGeteuid
	t.Cleanup(func() { osGeteuid = previousGeteuid })

	osGeteuid = func() int { return 1000 }
	t.Setenv("IS_SANDBOX", "")
	require.True(t, bypassPermissionsAvailable())
	require.True(t, modeAvailableForModel(modeBypassPermissions, "", nil))

	osGeteuid = func() int { return 0 }
	require.False(t, bypassPermissionsAvailable())
	require.False(t, modeAvailableForModel(modeBypassPermissions, "", nil))

	t.Setenv("IS_SANDBOX", "1")
	require.True(t, bypassPermissionsAvailable())
	require.True(t, modeAvailableForModel(modeBypassPermissions, "", nil))
}

func TestStartSessionFallsBackWhenBypassPermissionsUnavailable(t *testing.T) {
	previousGeteuid := osGeteuid
	t.Cleanup(func() { osGeteuid = previousGeteuid })

	osGeteuid = func() int { return 0 }
	t.Setenv("IS_SANDBOX", "")

	fake := newAgentFakeTransport()
	var captured claude.Options
	agent := NewAgent(WithDefaultPermissionMode(permissionModeBypassPermissions))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.Equal(t, string(modeDefault), captured.PermissionMode)

	session, err := agent.session(resp.SessionId)
	require.NoError(t, err)
	require.Equal(t, modeDefault, session.mode)
}

func TestAgentGatewayAuthEnv(t *testing.T) {
	t.Parallel()

	var captured []claude.Options
	var fakes []*agentFakeTransport
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithEnv(map[string]string{
		envAnthropicBaseURL: "https://user.example",
		"userEnv":           "user-value",
	}))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = append(captured, options)
		fake := newAgentFakeTransport()
		fakes = append(fakes, fake)

		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta: map[string]any{
			authMetaGateway: map[string]any{
				"baseUrl": "https://gateway.example",
				"headers": map[string]any{
					"z-api-key": "z",
					"a-api-key": "a",
					"ignored":   7,
				},
			},
		},
	})
	require.NoError(t, err)

	firstSession, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.Len(t, captured, 1)
	require.Equal(t, map[string]string{
		envAnthropicAuthToken: "",
		envAnthropicBaseURL:   "https://gateway.example",
		envAnthropicHeaders:   "a-api-key: a\nz-api-key: z",
		"userEnv":             "user-value",
	}, captured[0].Env)

	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.NoError(t, err)
	require.True(t, fakes[0].isClosed())
	_, err = agent.session(firstSession.SessionId)
	require.Error(t, err)

	_, err = agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta: map[string]any{
			authMetaGateway: map[string]any{
				"baseUrl": "https://gateway.example",
			},
		},
	})
	require.NoError(t, err)

	secondSession, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.Len(t, captured, 2)
	require.Equal(t, "", captured[1].Env[envAnthropicHeaders])

	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.NoError(t, err)
	require.True(t, fakes[1].isClosed())
	_, err = agent.session(secondSession.SessionId)
	require.Error(t, err)

	_, err = agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.Len(t, captured, 3)
	require.Equal(t, map[string]string{
		envAnthropicBaseURL: "https://user.example",
		"userEnv":           "user-value",
	}, captured[2].Env)
}

func TestAgentLogoutWithoutGatewayKeepsSessions(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.NoError(t, err)
	require.False(t, fake.isClosed())

	_, err = agent.session(session.SessionId)
	require.NoError(t, err)
}

func TestAgentLogoutKeepsPreGatewaySessions(t *testing.T) {
	t.Parallel()

	var fakes []*agentFakeTransport
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		fake := newAgentFakeTransport()
		fakes = append(fakes, fake)

		return claude.NewClient(nil, options, fake)
	}

	preGateway, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta: map[string]any{
			authMetaGateway: map[string]any{
				"baseUrl": "https://gateway.example",
			},
		},
	})
	require.NoError(t, err)

	gatewayBacked, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.NoError(t, err)
	require.False(t, fakes[0].isClosed())
	require.True(t, fakes[1].isClosed())

	_, err = agent.session(preGateway.SessionId)
	require.NoError(t, err)
	_, err = agent.session(gatewayBacked.SessionId)
	require.Error(t, err)
}

func TestAgentLogoutReturnsGatewaySessionCloseErrors(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta: map[string]any{
			authMetaGateway: map[string]any{
				"baseUrl": "https://gateway.example",
			},
		},
	})
	require.NoError(t, err)

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.mu.Lock()
	fake.closeErr = closeErr
	fake.mu.Unlock()

	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.ErrorIs(t, err, closeErr)

	_, err = agent.session(session.SessionId)
	require.Error(t, err)
}

func TestAgentGatewayAuthRejectsProcessMCP(t *testing.T) {
	t.Parallel()

	var starts int
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		starts++

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	_, err := agent.Authenticate(context.Background(), acp.AuthenticateRequest{
		MethodId: authMethodGateway,
		Meta: map[string]any{
			authMetaGateway: map[string]any{
				"baseUrl": "https://gateway.example",
				"headers": map[string]any{"authorization": "Bearer secret"},
			},
		},
	})
	require.NoError(t, err)

	expectGatewayMCPReject(t, agent, acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "server"}},
		},
	})
	expectGatewayMCPReject(t, agent, acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "server-1", Type: "acp"}},
		},
	})

	require.Zero(t, starts)
}

func expectGatewayMCPReject(t *testing.T, agent *Agent, req acp.NewSessionRequest) {
	t.Helper()

	_, err := agent.NewSession(context.Background(), req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gateway auth cannot be used")
}

func TestAgentIgnoresClaudeSettingsAndContextUsageErrors(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.controlErrors = map[string]string{
		"get_settings":      "settings failed",
		"get_context_usage": "usage failed",
	}

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
}

func TestAgentSessionOptionalUpdateErrors(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"
	initializeInfo := map[string]any{
		"commands": []any{map[string]any{"name": "debug", "description": "Debug session"}},
	}

	newAgentWithFailingConnection := func(claudeHome string) (*Agent, *agentFakeTransport) {
		fake := newAgentFakeTransport()
		fake.initializeInfo = initializeInfo
		agent := NewAgent(WithClaudeHome(claudeHome))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}
		attachFailingConnection(agent)

		return agent, fake
	}

	agent, fake := newAgentWithFailingConnection(t.TempDir())
	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)

	agent, fake = newAgentWithFailingConnection(t.TempDir())
	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})
	agent, fake = newAgentWithFailingConnection(claudeHome)
	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)

	agent, fake = newAgentWithFailingConnection(t.TempDir())
	_, err = agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)
}

func TestModelAndConfigHelpers(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{Value: "", DisplayName: "ignored"},
		{
			Value:                 "default",
			DisplayName:           "Default",
			Description:           "Recommended",
			SupportedEffortLevels: []string{"low", "", "medium", "xhigh", "medium"},
		},
		{Value: "default", DisplayName: "Duplicate"},
		{Value: "sonnet"},
	}

	require.Nil(t, configOptions("", "", available, "", nil, "", false, false))
	require.Nil(t, unstableConfigOptions("", "", available, "", nil, "", false, false))

	options := configOptions(modeDefault, "default", available, "default", []string{"default", "", "Explanatory", "default"}, "medium", true, true)
	require.Len(t, options, 5)
	require.Equal(t, acp.SessionConfigOptionCategoryModel, *options[0].Select.Category)
	values := *options[0].Select.Options.Ungrouped
	require.Len(t, values, 2)
	require.Equal(t, "Default", values[0].Name)
	require.Equal(t, "Recommended", *values[0].Description)
	require.Equal(t, claudeModelMetaForTest(map[string]any{
		claudeModelMetaSupportedEffortKey: []string{"low", "medium", "xhigh"},
	}), values[0].Meta)
	require.Equal(t, "sonnet", values[1].Name)
	require.Equal(t, claudeModelMetaForTest(map[string]any{
		claudeModelMetaContextWindowKey: defaultContextWindow,
	}), values[1].Meta)
	require.Equal(t, acp.SessionConfigValueId("default"), values[0].Value)
	require.Equal(t, configMode, options[1].Select.Id)
	require.Equal(t, acp.SessionConfigOptionCategoryMode, *options[1].Select.Category)
	require.Equal(t, acp.SessionConfigValueId(modeDefault), options[1].Select.CurrentValue)
	require.Equal(t, configOutputStyle, options[2].Select.Id)
	styleValues := *options[2].Select.Options.Ungrouped
	require.Equal(t, []acp.SessionConfigSelectOption{
		{Name: "default", Value: "default"},
		{Name: "Explanatory", Value: "Explanatory"},
	}, []acp.SessionConfigSelectOption(styleValues))
	require.Equal(t, configEffort, options[3].Select.Id)
	require.Equal(t, acp.SessionConfigOptionCategoryThoughtLevel, *options[3].Select.Category)
	effortValues := *options[3].Select.Options.Ungrouped
	require.Equal(t, []acp.SessionConfigSelectOption{
		{Name: "Low", Value: "low"},
		{Name: "Medium", Value: "medium"},
		{Name: "Extra High", Value: "xhigh"},
	}, []acp.SessionConfigSelectOption(effortValues))
	require.Equal(t, configFastMode, options[4].Boolean.Id)
	require.Equal(t, modelConfigCategory, *options[4].Boolean.Category)
	require.Equal(t, configTypeBoolean, options[4].Boolean.Type)
	require.True(t, options[4].Boolean.CurrentValue)

	unstableOptions := unstableConfigOptions(modeDefault, "custom", available, "custom-style", nil, "medium", false, true)
	require.Len(t, unstableOptions, 4)
	require.Equal(t, acp.SessionConfigOptionCategoryModel, *unstableOptions[0].Select.Category)
	require.Equal(t, acp.SessionConfigValueId("custom"), unstableOptions[0].Select.CurrentValue)
	require.Equal(t, acp.SessionConfigValueId(modeDefault), unstableOptions[1].Select.CurrentValue)
	require.Equal(t, acp.SessionConfigValueId("custom-style"), unstableOptions[2].Select.CurrentValue)
	require.Equal(t, configFastMode, unstableOptions[3].Boolean.Id)
	require.Equal(t, configTypeBoolean, unstableOptions[3].Boolean.Type)
	require.False(t, unstableOptions[3].Boolean.CurrentValue)

	unstableOptions = unstableConfigOptions(modeDefault, "default", available, "", nil, effortMax, false, false)
	require.Len(t, unstableOptions, 3)
	require.Equal(t, acp.SessionConfigOptionCategoryThoughtLevel, *unstableOptions[2].Select.Category)
	require.Equal(t, acp.SessionConfigValueId(effortMax), unstableOptions[2].Select.CurrentValue)

	require.Equal(t, "High", effortDisplayName("high"))
	require.Equal(t, "Max", effortDisplayName(effortMax))
	require.Equal(t, "custom", effortDisplayName("custom"))
	fallbackEffort, changed := reconcileEffortForModel("fallback", []claude.AvailableModelInfo{
		{Value: "fallback", SupportedEffortLevels: []string{"low"}},
	}, "medium")
	require.True(t, changed)
	require.Equal(t, "low", fallbackEffort)
}

func TestClaudeModelVariantMetaAndContextHints(t *testing.T) {
	t.Parallel()

	opus := claude.AvailableModelInfo{
		Value:                 "opus",
		DisplayName:           "Opus",
		Description:           "Opus 4.7 with 1M context",
		SupportedEffortLevels: []string{"low", "", effortMax},
		SupportsAutoMode:      true,
	}
	meta := claudeModelVariantMeta("opus", []claude.AvailableModelInfo{opus}, effortMax)
	claudeMeta, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "opus", claudeMeta[claudeModelMetaModelIDKey])
	require.Equal(t, effortMax, claudeMeta[claudeModelMetaVariantKey])
	require.Equal(t, []string{"low", effortMax}, claudeMeta[claudeModelMetaAvailableVariantsKey])
	require.Nil(t, claudeModelVariantMeta("", nil, ""))

	noVariant := claudeModelVariantMeta("sonnet", nil, "")
	noVariantClaude, ok := noVariant[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Nil(t, noVariantClaude[claudeModelMetaVariantKey])
	require.Empty(t, noVariantClaude[claudeModelMetaAvailableVariantsKey])

	require.Equal(t, claudeModelMetaForTest(map[string]any{
		claudeModelMetaContextWindowKey:    largeContextWindow,
		claudeModelMetaSupportedEffortKey:  []string{"low", effortMax},
		claudeModelMetaSupportsAutoModeKey: true,
	}), claudeModelInfoMeta(opus))
	require.Nil(t, claudeModelInfoMeta(claude.AvailableModelInfo{Value: "custom"}))

	require.Equal(t, largeContextWindow, modelContextWindowHint(claude.AvailableModelInfo{Value: "sonnet[1m]"}))
	require.Equal(t, largeContextWindow, contextWindowForAvailableModel("default", []claude.AvailableModelInfo{
		{Value: "default", Description: "Opus 4.7 with 1 million token context"},
	}))
	require.Equal(t, largeContextWindow, contextWindowForAvailableModel("opus", nil))
	require.Equal(t, defaultContextWindow, contextWindowForAvailableModel("sonnet", nil))
	require.Equal(t, defaultContextWindow, contextWindowForAvailableModel("unknown", nil))

	haiku := claude.AvailableModelInfo{Value: "haiku", DisplayName: "Haiku"}
	require.Equal(t, claudeModelFamilyHaiku, modelFamily(haiku))
	require.Empty(t, modelFamily(claude.AvailableModelInfo{Value: "custom"}))

	info, ok := availableModelInfo("opus", []claude.AvailableModelInfo{opus})
	require.True(t, ok)
	require.Equal(t, opus, info)
	_, ok = availableModelInfo("missing", []claude.AvailableModelInfo{opus})
	require.False(t, ok)
	require.Equal(t, []string{"a", "b"}, nonEmptyModelStrings([]string{"", "a", "", "b"}))
}

func TestAgentACPConnectionStreamsUpdates(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.assistantText = "streamed"

	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 3
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.Equal(t, "streamed", updates[0].Update.AgentMessageChunk.Content.Text.Text)
	require.Equal(t, 0.01, updates[1].Update.UsageUpdate.Cost.Amount)
	require.Equal(t, 200000, updates[1].Update.UsageUpdate.Size)
	require.Equal(t, 13, updates[1].Update.UsageUpdate.Used)
	require.NotNil(t, updates[2].Update.SessionInfoUpdate)
	require.NotNil(t, updates[2].Update.SessionInfoUpdate.Title)
	require.Equal(t, "hello", *updates[2].Update.SessionInfoUpdate.Title)

	_, err = setModeConfig(ctx, conn, session.SessionId, modePlan)
	require.NoError(t, err)

	_, err = conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  configModel,
			Value:     "claude-next",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 5
	}, time.Second, 10*time.Millisecond)

	updates = client.recordedUpdates()
	require.Equal(t, acp.SessionConfigValueId(modePlan), updates[3].Update.ConfigOptionUpdate.ConfigOptions[1].Select.CurrentValue)
	require.Equal(t, acp.SessionConfigValueId("claude-next"), updates[4].Update.ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)

	_, err = setModelConfig(ctx, conn, session.SessionId, "claude-opus")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 6
	}, time.Second, 10*time.Millisecond)

	updates = client.recordedUpdates()
	require.Equal(t, acp.SessionConfigValueId("claude-opus"), updates[5].Update.ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)
}

func TestLocalAgentPromptQueueing(t *testing.T) {
	t.Parallel()

	t.Run("queued prompt can be cancelled before it runs", func(t *testing.T) {
		t.Parallel()

		_, fake, conn := newQueueingAgentForTest(t)
		session := newQueueingSessionForTest(t, conn)

		activeCtx, activeCancel := context.WithCancel(context.Background())
		defer activeCancel()

		activePrompt := promptAsyncRaw(conn, activeCtx, session.SessionId, "first")
		require.Eventually(t, func() bool {
			return len(sentUserPayloads(fake)) == 1
		}, time.Second, 10*time.Millisecond)

		queuedCtx, queuedCancel := context.WithCancel(context.Background())
		queuedPrompt := promptAsyncRaw(conn, queuedCtx, session.SessionId, "second")
		require.Never(t, func() bool {
			return len(sentUserPayloads(fake)) > 1
		}, 100*time.Millisecond, 10*time.Millisecond)

		queuedCancel()
		queuedResult := requirePromptResult(t, queuedPrompt)
		if queuedResult.err == nil {
			require.Equal(t, acp.StopReasonCancelled, queuedResult.resp.StopReason)
		}
		require.Len(t, sentUserPayloads(fake), 1)

		activeCancel()
		activeResult := requirePromptResult(t, activePrompt)
		if activeResult.err == nil {
			require.Equal(t, acp.StopReasonCancelled, activeResult.resp.StopReason)
		}
	})

	t.Run("active prompt cancellation releases next queued prompt", func(t *testing.T) {
		t.Parallel()

		_, fake, conn := newQueueingAgentForTest(t)
		session := newQueueingSessionForTest(t, conn)

		activePrompt := promptAsyncRaw(conn, context.Background(), session.SessionId, "first")
		require.Eventually(t, func() bool {
			return len(sentUserPayloads(fake)) == 1
		}, time.Second, 10*time.Millisecond)

		queuedPrompt := promptAsyncRaw(conn, context.Background(), session.SessionId, "second")
		require.Never(t, func() bool {
			return len(sentUserPayloads(fake)) > 1
		}, 100*time.Millisecond, 10*time.Millisecond)

		emitResultOnInterrupt(fake, map[string]any{
			"type":        "result",
			"subtype":     "error_during_execution",
			"is_error":    true,
			"stop_reason": nil,
			"errors":      []any{"[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"},
		})
		fake.setSuppressResult(false)

		require.NoError(t, conn.SendNotification(context.Background(), acp.AgentMethodSessionCancel, acp.CancelNotification{
			SessionId: session.SessionId,
		}))

		activeResult := requirePromptResult(t, activePrompt)
		require.NoError(t, activeResult.err)
		require.Equal(t, acp.StopReasonCancelled, activeResult.resp.StopReason)

		queuedResult := requirePromptResult(t, queuedPrompt)
		require.NoError(t, queuedResult.err)
		require.Equal(t, acp.StopReasonEndTurn, queuedResult.resp.StopReason)
		require.Len(t, sentUserPayloads(fake), 2)
	})

	t.Run("different sessions run concurrently", func(t *testing.T) {
		t.Parallel()

		var mu sync.Mutex
		var fakes []*agentFakeTransport
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			fake := newAgentFakeTransport()
			fake.suppressResult = true

			mu.Lock()
			fakes = append(fakes, fake)
			mu.Unlock()

			return claude.NewClient(nil, options, fake)
		}

		rawClient := &rawForwardACPClient{}
		conn := connectAgentRawForTest(t, agent, rawClient.handle)
		sessionA := newQueueingSessionForTest(t, conn)
		sessionB := newQueueingSessionForTest(t, conn)

		mu.Lock()
		require.Len(t, fakes, 2)
		fakeA := fakes[0]
		fakeB := fakes[1]
		mu.Unlock()

		emitResultOnInterrupt(fakeA, successResultMessage())
		emitResultOnInterrupt(fakeB, successResultMessage())

		promptA := promptAsyncRaw(conn, context.Background(), sessionA.SessionId, "first")
		promptB := promptAsyncRaw(conn, context.Background(), sessionB.SessionId, "second")

		require.Eventually(t, func() bool {
			return len(sentUserPayloads(fakeA)) == 1 && len(sentUserPayloads(fakeB)) == 1
		}, time.Second, 10*time.Millisecond)

		require.NoError(t, conn.SendNotification(context.Background(), acp.AgentMethodSessionCancel, acp.CancelNotification{
			SessionId: sessionA.SessionId,
		}))
		require.NoError(t, conn.SendNotification(context.Background(), acp.AgentMethodSessionCancel, acp.CancelNotification{
			SessionId: sessionB.SessionId,
		}))

		resultA := requirePromptResult(t, promptA)
		require.NoError(t, resultA.err)
		require.Equal(t, acp.StopReasonCancelled, resultA.resp.StopReason)

		resultB := requirePromptResult(t, promptB)
		require.NoError(t, resultB.err)
		require.Equal(t, acp.StopReasonCancelled, resultB.resp.StopReason)
	})
}

func TestAgentACPConnectionHandlesPermission(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{permission: "allow_once"}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "perm-1",
		"request": map[string]any{
			"subtype":     "can_use_tool",
			"tool_name":   "Read",
			"tool_use_id": "tool-1",
			"input":       map[string]any{"file_path": "/tmp/a"},
		},
	}

	require.Eventually(t, func() bool {
		return len(client.recordedPermissions()) == 1
	}, time.Second, 10*time.Millisecond)

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		for _, payload := range fake.sentPayloads() {
			resp, ok := payload.(claude.ControlResponse)
			if ok && resp.Response["request_id"] == "perm-1" {
				response = resp

				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond)

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, payload["behavior"])
	require.Equal(t, map[string]any{"file_path": "/tmp/a"}, payload["updatedInput"])
}

func TestAgentPermissionSuggestionsRoundTrip(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{permission: permissionAllowAlways}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	sessionResp, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	suggestion := map[string]any{
		jsonFieldType:               permissionUpdateAddRules,
		permissionUpdateBehavior:    claude.BehaviorAllow,
		permissionUpdateDestination: permissionUpdateSession,
		permissionUpdateRules: []any{
			map[string]any{
				permissionUpdateToolName:    "Bash",
				permissionUpdateRuleContent: "git status",
			},
		},
	}
	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "perm-suggestion",
		"request": map[string]any{
			"subtype":     "can_use_tool",
			"tool_name":   "Bash",
			"tool_use_id": "tool-1",
			"input":       map[string]any{"command": "git status"},
			"suggestions": []any{suggestion},
		},
	}

	require.Eventually(t, func() bool {
		return len(client.recordedPermissions()) == 1
	}, time.Second, 10*time.Millisecond)

	request := client.recordedPermissions()[0]
	require.Equal(t, "Always Allow Bash(git status)", request.Options[1].Name)

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		for _, payload := range fake.sentPayloads() {
			resp, ok := payload.(claude.ControlResponse)
			if ok && resp.Response["request_id"] == "perm-suggestion" {
				response = resp

				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond)

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []map[string]any{suggestion}, payload["updatedPermissions"])

	session, err := agent.session(sessionResp.SessionId)
	require.NoError(t, err)
	_, saved := session.permissionRule("Bash")
	require.False(t, saved)
}

func TestAgentPermissionClientError(t *testing.T) {
	t.Parallel()

	permissionErr := errors.New("permission failed")
	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{permissionErr: permissionErr}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- permissionRequest("perm-error-1", "Read")

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "perm-error-1", &response)
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "error", response.Response["subtype"])
	require.Contains(t, response.Response["error"], "permission failed")
}

func TestAgentDocumentNotificationsAddPromptContext(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	err = agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId:  session.SessionId,
		Uri:        "file:///repo/main.go",
		LanguageId: "go",
		Text:       "package main\n",
		Version:    1,
	})
	require.NoError(t, err)

	err = agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/main.go",
		Version:   2,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 0, Character: 8},
					End:   acp.UnstablePosition{Line: 0, Character: 12},
				},
				Text: "changed",
			},
		},
	})
	require.NoError(t, err)

	err = agent.UnstableDidSaveDocument(context.Background(), acp.UnstableDidSaveDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/main.go",
	})
	require.NoError(t, err)

	promptResp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("summarize the focused file")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, promptResp.StopReason)

	userPayloads := sentUserPayloads(fake)
	require.Len(t, userPayloads, 1)

	message, ok := userPayloads[0]["message"].(map[string]any)
	require.True(t, ok)
	content, ok := message["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 2)

	contextText, _ := content[0]["text"].(string)
	require.Contains(t, contextText, "file:///repo/main.go")
	require.Contains(t, contextText, "package changed")
	require.Contains(t, contextText, "Saved: true")

	require.NoError(t, agent.UnstableDidCloseDocument(context.Background(), acp.UnstableDidCloseDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/main.go",
	}))
	require.Empty(t, agent.documentContext(session.SessionId))
}

func TestAgentDocumentNoOpChangesDoNotBumpVersion(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	const uri = "file:///repo/main.go"
	require.NoError(t, agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId:  session.SessionId,
		Uri:        uri,
		LanguageId: "go",
		Text:       "package main\n",
		Version:    1,
	}))

	require.NoError(t, agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       uri,
		Version:   2,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 0, Character: 0},
					End:   acp.UnstablePosition{Line: 0, Character: 0},
				},
				Text: "",
			},
		},
	}))

	agent.docsMu.Lock()
	document := agent.documents[session.SessionId][uri]
	agent.docsMu.Unlock()
	require.Equal(t, 1, document.Version)
	require.True(t, document.Saved)
	require.Equal(t, "package main\n", document.Text)

	require.NoError(t, agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       uri,
		Version:   3,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{Text: "package main\n"},
		},
	}))

	agent.docsMu.Lock()
	document = agent.documents[session.SessionId][uri]
	agent.docsMu.Unlock()
	require.Equal(t, 1, document.Version)
	require.True(t, document.Saved)

	require.NoError(t, agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       uri,
		Version:   4,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 0, Character: 0},
					End:   acp.UnstablePosition{Line: 0, Character: 0},
				},
				Text: "// generated\n",
			},
		},
	}))

	agent.docsMu.Lock()
	document = agent.documents[session.SessionId][uri]
	agent.docsMu.Unlock()
	require.Equal(t, 4, document.Version)
	require.False(t, document.Saved)
	require.Equal(t, "// generated\npackage main\n", document.Text)
}

func TestAgentDocumentOpenAllowsEmptyText(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	err = agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/empty.txt",
		Text:      "",
		Version:   1,
	})
	require.NoError(t, err)
	require.Contains(t, agent.documentContext(session.SessionId), "Document: file:///repo/empty.txt")
}

func TestAgentDocumentFocusSelectsContext(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	require.NoError(t, agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/a.go",
		Text:      "package a\n",
		Version:   1,
	}))
	require.NoError(t, agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/b.go",
		Text:      "package b\n",
		Version:   1,
	}))

	require.NoError(t, agent.UnstableDidFocusDocument(context.Background(), acp.UnstableDidFocusDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/a.go",
		Version:   7,
	}))

	contextText := agent.documentContext(session.SessionId)
	require.Contains(t, contextText, "Document: file:///repo/a.go")
	require.Contains(t, contextText, "Version: 7")
	require.NotContains(t, contextText, "file:///repo/b.go")
}

func TestAgentDocumentSaveFocusAndCloseEdges(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	require.NoError(t, agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/new.go",
		Version:   1,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{Text: "package main\n"},
		},
	}))
	require.Contains(t, agent.documentContext(session.SessionId), "file:///repo/new.go")

	require.NoError(t, agent.UnstableDidSaveDocument(context.Background(), acp.UnstableDidSaveDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/new.go",
	}))
	require.Contains(t, agent.documentContext(session.SessionId), "Saved: true")

	require.NoError(t, agent.UnstableDidFocusDocument(context.Background(), acp.UnstableDidFocusDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/missing.go",
		Version:   2,
	}))
	require.Contains(t, agent.documentContext(session.SessionId), "file:///repo/new.go")

	require.NoError(t, agent.UnstableDidCloseDocument(context.Background(), acp.UnstableDidCloseDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/missing.go",
	}))
	require.Contains(t, agent.documentContext(session.SessionId), "file:///repo/new.go")

	require.NoError(t, agent.UnstableDidFocusDocument(context.Background(), acp.UnstableDidFocusDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/new.go",
		Version:   3,
	}))
	require.NoError(t, agent.UnstableDidCloseDocument(context.Background(), acp.UnstableDidCloseDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/new.go",
	}))
	require.Empty(t, agent.documentContext(session.SessionId))
}

func TestAgentDocumentValidationErrors(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))

	require.Error(t, agent.UnstableDidChangeDocument(context.Background(), acp.UnstableDidChangeDocumentNotification{}))
	require.Error(t, agent.UnstableDidCloseDocument(context.Background(), acp.UnstableDidCloseDocumentNotification{}))
	require.Error(t, agent.UnstableDidFocusDocument(context.Background(), acp.UnstableDidFocusDocumentNotification{}))
	require.Error(t, agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{}))
	require.Error(t, agent.UnstableDidSaveDocument(context.Background(), acp.UnstableDidSaveDocumentNotification{}))
}

func TestAgentPermissionAlwaysCachesRule(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{permission: permissionAllowAlways}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- permissionRequest("perm-1", "Read")
	require.Eventually(t, func() bool {
		return len(client.recordedPermissions()) == 1
	}, time.Second, 10*time.Millisecond)

	var first claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "perm-1", &first)
	}, time.Second, 10*time.Millisecond)

	firstPayload, ok := first.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, firstPayload["behavior"])
	require.NotEmpty(t, firstPayload["updatedPermissions"])

	fake.incoming <- permissionRequest("perm-2", "Read")

	var second claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "perm-2", &second)
	}, time.Second, 10*time.Millisecond)

	require.Len(t, client.recordedPermissions(), 1)

	secondPayload, ok := second.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, secondPayload["behavior"])
}

func TestAgentPermissionAlwaysPersistsAcrossResume(t *testing.T) {
	t.Parallel()

	claudeHome := t.TempDir()
	fake1 := newAgentFakeTransport()
	agent1 := NewAgent(WithClaudeHome(claudeHome))
	agent1.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake1)
	}

	client1 := &recordingACPClient{permission: permissionAllowAlways}
	conn1 := connectAgentForTest(t, agent1, client1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := conn1.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	session, err := conn1.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake1.incoming <- permissionRequest("persist-1", "Read")

	var first claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake1, "persist-1", &first)
	}, time.Second, 10*time.Millisecond)

	require.Len(t, client1.recordedPermissions(), 1)

	fake2 := newAgentFakeTransport()
	agent2 := NewAgent(WithClaudeHome(claudeHome))
	agent2.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, string(session.SessionId), options.ResumeID)

		return claude.NewClient(nil, options, fake2)
	}

	client2 := &recordingACPClient{permission: permissionRejectOnce}
	conn2 := connectAgentForTest(t, agent2, client2)

	_, err = conn2.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn2.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	fake2.incoming <- permissionRequest("persist-2", "Read")

	var second claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake2, "persist-2", &second)
	}, time.Second, 10*time.Millisecond)

	require.Empty(t, client2.recordedPermissions())

	payload, ok := second.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, payload["behavior"])
}

func TestAgentPermissionRulesForSessionBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.sessions["active"] = &Session{permissionRules: map[string]string{"Read": claude.BehaviorAllow}}

	rules, err := agent.permissionRulesForSession(context.Background(), "active")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow}, rules)

	home := t.TempDir()
	store := permissions.Store{ClaudeHome: home}
	require.NoError(t, store.Save(context.Background(), "cached", map[string]string{"Write": claude.BehaviorDeny}))

	agent = NewAgent(WithClaudeHome(home))
	rules, err = agent.permissionRulesForSession(context.Background(), "cached")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Write": claude.BehaviorDeny}, rules)

	storeDir := filepath.Join(home, "acp-go-claude")
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, "session-permissions.json"), []byte("{bad"), 0o600))

	rules, err = agent.permissionRulesForSession(context.Background(), "cached")
	require.NoError(t, err)
	require.Empty(t, rules)

	rules, err = agent.permissionRulesForSession(context.Background(), "missing")
	require.NoError(t, err)
	require.Empty(t, rules)

	readErrHome := t.TempDir()
	readErrStoreDir := filepath.Join(readErrHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(readErrStoreDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(readErrStoreDir, "session-permissions.json"), 0o700))

	agent = NewAgent(WithClaudeHome(readErrHome))
	_, err = agent.permissionRulesForSession(context.Background(), "missing")
	require.Error(t, err)

	agent.cachePermissionRules("cached", map[string]string{"Write": claude.BehaviorDeny})
	rules, err = agent.permissionRulesForSession(context.Background(), "cached")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Write": claude.BehaviorDeny}, rules)
}

func TestAgentPermissionCacheClearedWithSessionLifecycle(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	session := &Session{
		agent:  agent,
		id:     "session-1",
		client: claude.NewClient(nil, claude.Options{}, newAgentFakeTransport()),
		turn:   make(chan struct{}, 1),
	}
	agent.sessions[session.id] = session
	agent.cachePermissionRules(session.id, map[string]string{"Read": claude.BehaviorAllow})

	agent.removeSession(context.Background(), session.id, session)
	_, ok := agent.cachedPermissionRules(session.id)
	require.False(t, ok)

	closeSession := &Session{
		agent:  agent,
		id:     "session-2",
		client: claude.NewClient(nil, claude.Options{}, newAgentFakeTransport()),
		turn:   make(chan struct{}, 1),
	}
	agent.sessions[closeSession.id] = closeSession
	agent.cachePermissionRules(closeSession.id, map[string]string{"Write": claude.BehaviorDeny})
	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: closeSession.id})
	require.NoError(t, err)
	_, ok = agent.cachedPermissionRules(closeSession.id)
	require.False(t, ok)

	gatewaySession := &Session{
		agent:       agent,
		id:          "session-3",
		client:      claude.NewClient(nil, claude.Options{}, newAgentFakeTransport()),
		turn:        make(chan struct{}, 1),
		gatewayAuth: true,
	}
	agent.sessions[gatewaySession.id] = gatewaySession
	agent.gatewayAuth = &gatewayAuth{BaseURL: "https://example.com"}
	agent.cachePermissionRules(gatewaySession.id, map[string]string{"Edit": claude.BehaviorAllow})
	require.Len(t, agent.clearGatewayAuthForLogout(), 1)
	_, ok = agent.cachedPermissionRules(gatewaySession.id)
	require.False(t, ok)

	agent.cachePermissionRules("session-4", map[string]string{"Bash": claude.BehaviorDeny})
	require.NoError(t, agent.Close())
	_, ok = agent.cachedPermissionRules("session-4")
	require.False(t, ok)
}

func TestAgentSessionStartRejectedAfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	closedAgent := NewAgent(WithClaudeHome(t.TempDir()))
	require.NoError(t, closedAgent.Close())

	_, err := closedAgent.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.ErrorIs(t, err, errAgentClosed)

	_, err = closedAgent.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  "11111111-1111-4111-8111-111111111111",
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, errAgentClosed)

	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	sessionID := acp.SessionId("22222222-2222-4222-8222-222222222222")
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: string(sessionID)}, []SessionStoreEntry{
		json.RawMessage(`{"type":"user","message":{"content":"hello"}}`),
	}))

	closedLoadAgent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	require.NoError(t, closedLoadAgent.Close())
	_, err = closedLoadAgent.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, errAgentClosed)
}

func TestAgentSessionStartClosedBeforeInsert(t *testing.T) {
	t.Parallel()

	makeAgent := func(t *testing.T) (*Agent, *agentFakeTransport) {
		t.Helper()

		agent := NewAgent(WithClaudeHome(t.TempDir()))
		fake := newAgentFakeTransport()
		fake.setSendHook(func(payload any) {
			req, ok := payload.(claude.ControlRequest)
			if !ok {
				return
			}

			if subtype, _ := req.Request["subtype"].(string); subtype == "get_settings" {
				_ = agent.Close()
			}
		})
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		return agent, fake
	}

	t.Run("new", func(t *testing.T) {
		t.Parallel()

		agent, fake := makeAgent(t)
		_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
		require.ErrorIs(t, err, errAgentClosed)
		require.True(t, fake.isClosed())
	})

	t.Run("resume", func(t *testing.T) {
		t.Parallel()

		agent, fake := makeAgent(t)
		_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
			SessionId:  "33333333-3333-4333-8333-333333333333",
			Cwd:        t.TempDir(),
			McpServers: []acp.McpServer{},
		})
		require.ErrorIs(t, err, errAgentClosed)
		require.True(t, fake.isClosed())
	})

	t.Run("load", func(t *testing.T) {
		t.Parallel()

		store := NewInMemorySessionStore()
		cwd := t.TempDir()
		projectKey, err := projectKeyForDirectory(cwd)
		require.NoError(t, err)
		sessionID := acp.SessionId("44444444-4444-4444-8444-444444444444")
		require.NoError(t, store.Append(context.Background(), SessionKey{
			ProjectKey: projectKey,
			SessionID:  string(sessionID),
		}, []SessionStoreEntry{json.RawMessage(`{"type":"user","message":{"content":"hello"}}`)}))

		agent, fake := makeAgent(t)
		agent.options.SessionStore = store

		_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
			SessionId:  sessionID,
			Cwd:        cwd,
			McpServers: []acp.McpServer{},
		})
		require.ErrorIs(t, err, errAgentClosed)
		require.True(t, fake.isClosed())
	})
}

func TestAgentStoreStartedSessionClosedCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	fake := newAgentFakeTransport()
	fake.closeErr = closeErr

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.cachePermissionRules("session-1", map[string]string{"Read": claude.BehaviorAllow})
	require.NoError(t, agent.Close())

	err := agent.storeStartedSession(context.Background(), &Session{
		agent:  agent,
		id:     "session-1",
		client: claude.NewClient(nil, claude.Options{}, fake),
		turn:   make(chan struct{}, 1),
	})
	require.ErrorIs(t, err, errAgentClosed)
	require.True(t, fake.isClosed())
	_, ok := agent.cachedPermissionRules("session-1")
	require.False(t, ok)
}

func TestAgentNewSessionRecoversCorruptPermissionRules(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	storeDir := filepath.Join(home, "acp-go-claude")
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, "session-permissions.json"), []byte("{bad"), 0o600))

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(home))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	_, _ = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: resp.SessionId})

	bridgeHome := t.TempDir()
	bridgeStoreDir := filepath.Join(bridgeHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(bridgeStoreDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bridgeStoreDir, "session-permissions.json"), []byte("{bad"), 0o600))

	bridgeFake := newAgentFakeTransport()
	bridgeAgent := NewAgent(WithClaudeHome(bridgeHome), WithMCPProxyCommand("proxy"))
	bridgeAgent.setConnection(&stubAgentClient{})
	bridgeAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, bridgeFake)
	}

	bridgeResp, err := bridgeAgent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.NoError(t, err)
	_, _ = bridgeAgent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: bridgeResp.SessionId})

	readErrHome := t.TempDir()
	readErrStoreDir := filepath.Join(readErrHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(readErrStoreDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(readErrStoreDir, "session-permissions.json"), 0o700))

	readErrAgent := NewAgent(WithClaudeHome(readErrHome))
	readErrAgent.newClaudeClient = func(*slog.Logger, claude.Options) *claude.Client {
		t.Fatal("Claude client should not start when permission rules cannot be read")

		return nil
	}
	_, err = readErrAgent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "load permission rules")

	bridgeReadErrHome := t.TempDir()
	bridgeReadErrStoreDir := filepath.Join(bridgeReadErrHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(bridgeReadErrStoreDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(bridgeReadErrStoreDir, "session-permissions.json"), 0o700))

	bridgeReadErrAgent := NewAgent(WithClaudeHome(bridgeReadErrHome), WithMCPProxyCommand("proxy"))
	bridgeReadErrAgent.setConnection(&stubAgentClient{})
	bridgeReadErrAgent.newClaudeClient = func(*slog.Logger, claude.Options) *claude.Client {
		t.Fatal("Claude client should not start when permission rules cannot be read")

		return nil
	}
	_, err = bridgeReadErrAgent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "load permission rules")
}

func TestAgentAskUserQuestionUsesElicitation(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{
		elicitationResponse: acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{
				Action: claude.ElicitationActionAccept,
				Content: map[string]any{
					"question_1": "Go",
					"q2":         []any{"fast", "safe"},
				},
			},
		},
	}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{
				Form: &acp.ElicitationFormCapabilities{},
			},
		},
	})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "ask-1",
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": askUserQuestionTool,
			"input": map[string]any{
				"questions": []any{
					map[string]any{
						"question": "Pick a language",
						"header":   "Language",
						"options": []any{
							map[string]any{"label": "Go", "description": "Simple"},
							map[string]any{"label": "Rust", "description": "Strict"},
						},
					},
					map[string]any{
						"id":          "q2",
						"question":    "Pick traits",
						"multiSelect": true,
						"options": []any{
							map[string]any{"label": "fast"},
							map[string]any{"label": "safe"},
						},
					},
				},
			},
		},
	}

	require.Eventually(t, func() bool {
		return len(client.recordedElicitations()) == 1
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, client.recordedPermissions())

	request := client.recordedElicitations()[0]
	require.NotNil(t, request.Form)
	require.Equal(t, "Claude needs more input.", request.Form.Message)
	require.Contains(t, request.Form.RequestedSchema.Required, "question_1")
	require.Contains(t, request.Form.RequestedSchema.Required, "q2")

	properties := request.Form.RequestedSchema.Properties
	firstProperty, ok := properties["question_1"].(map[string]any)
	require.True(t, ok)
	secondProperty, ok := properties["q2"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "string", firstProperty[jsonFieldType])
	require.Equal(t, "array", secondProperty[jsonFieldType])

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "ask-1", &response)
	}, time.Second, 10*time.Millisecond)

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, payload["behavior"])

	updatedInput, ok := payload["updatedInput"].(map[string]any)
	require.True(t, ok)
	answers, ok := updatedInput["answers"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Go", answers["Pick a language"])
	require.Equal(t, "fast, safe", answers["Pick traits"])
	require.NotContains(t, answers, "question_1")
	require.NotContains(t, answers, "q2")
}

func TestAgentAskUserQuestionDeniedWithoutElicitation(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{permission: permissionAllowOnce}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "ask-denied-1",
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": askUserQuestionTool,
			"input": map[string]any{
				"questions": []any{map[string]any{"question": "Pick one"}},
			},
		},
	}

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "ask-denied-1", &response)
	}, time.Second, 10*time.Millisecond)

	require.Empty(t, client.recordedElicitations())
	require.Empty(t, client.recordedPermissions())

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorDeny, payload["behavior"])
	require.Contains(t, payload["message"], "form elicitation")
}

func TestAgentACPConnectionHandlesElicitation(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{
		elicitationResponse: acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{
				Action:  claude.ElicitationActionAccept,
				Content: map[string]any{"approved": true},
			},
		},
	}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{
				Form: &acp.ElicitationFormCapabilities{},
			},
		},
	})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "elicitation-1",
		"request": map[string]any{
			"subtype":         "elicitation",
			"mcp_server_name": "server-1",
			"message":         "Approve access?",
			"mode":            "form",
			"tool_use_id":     "tool-1",
			"requested_schema": map[string]any{
				"description": "Approval form",
				"title":       "Approval",
				"type":        "object",
				"required":    []any{"approved"},
				"properties": map[string]any{
					"approved": map[string]any{"type": "boolean"},
				},
			},
		},
	}

	require.Eventually(t, func() bool {
		return len(client.recordedElicitations()) == 1
	}, time.Second, 10*time.Millisecond)

	request := client.recordedElicitations()[0]
	require.NotNil(t, request.Form)
	require.Equal(t, "Approve access?", request.Form.Message)
	require.NotNil(t, request.Form.RequestedSchema.Description)
	require.Equal(t, "Approval form", *request.Form.RequestedSchema.Description)
	require.Equal(t, []string{"approved"}, request.Form.RequestedSchema.Required)
	require.NotNil(t, request.Form.Meta[acpMetaKey])

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "elicitation-1", &response)
	}, time.Second, 10*time.Millisecond)

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.ElicitationActionAccept, payload["action"])
	require.Equal(t, map[string]any{"approved": true}, payload["content"])
}

func TestAgentElicitationClientError(t *testing.T) {
	t.Parallel()

	elicitationErr := errors.New("elicitation failed")
	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{elicitationErr: elicitationErr}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "elicitation-error-1",
		"request": map[string]any{
			"subtype": "elicitation",
			"message": "Approve?",
			"mode":    "form",
		},
	}

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "elicitation-error-1", &response)
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "error", response.Response["subtype"])
	require.Contains(t, response.Response["error"], "elicitation failed")
}

func TestAgentElicitationCompleteNotification(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":            "system",
			"subtype":         elicitationComplete,
			"elicitation_id":  "elicitation-1",
			"mcp_server_name": "server-1",
		},
	}

	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{
				Url: &acp.ElicitationUrlCapabilities{},
			},
		},
	})
	require.NoError(t, err)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	require.Eventually(t, func() bool {
		return len(client.recordedElicitationCompletions()) == 1
	}, time.Second, 10*time.Millisecond)

	completions := client.recordedElicitationCompletions()
	require.Equal(t, acp.UnstableElicitationId("elicitation-1"), completions[0].ElicitationId)
}

func TestAgentACPConnectionHandlesURLElicitation(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{
		elicitationResponse: acp.UnstableCreateElicitationResponse{
			Cancel: &acp.UnstableCreateElicitationCancel{Action: claude.ElicitationActionCancel},
		},
	}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{
				Url: &acp.ElicitationUrlCapabilities{},
			},
		},
	})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "url-elicitation-1",
		"request": map[string]any{
			"subtype": "elicitation",
			"message": "Authenticate",
			"mode":    "url",
			"url":     "https://example.com/auth",
		},
	}

	require.Eventually(t, func() bool {
		return len(client.recordedElicitations()) == 1
	}, time.Second, 10*time.Millisecond)

	request := client.recordedElicitations()[0]
	require.NotNil(t, request.Url)
	require.Equal(t, "https://example.com/auth", request.Url.Url)
	require.NotEmpty(t, request.Url.ElicitationId)

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "url-elicitation-1", &response)
	}, time.Second, 10*time.Millisecond)

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.ElicitationActionCancel, payload["action"])
}

func TestAgentElicitationDeclinesWithoutCapability(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	conn := connectAgentForTest(t, agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "elicitation-decline-1",
		"request": map[string]any{
			"subtype": "elicitation",
			"message": "Approve access?",
			"mode":    "form",
		},
	}

	var response claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "elicitation-decline-1", &response)
	}, time.Second, 10*time.Millisecond)

	require.Empty(t, client.recordedElicitations())

	payload, ok := response.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.ElicitationActionDecline, payload["action"])
}

func TestAgentListAndCloseSession(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, resp.SessionId, list.Sessions[0].SessionId)

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: resp.SessionId})
	require.NoError(t, err)
	require.True(t, fake.isClosed())

	require.Eventually(t, func() bool {
		list, listErr := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})

		return listErr == nil && len(list.Sessions) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestAgentListSessionsFiltersDedupeAndErrors(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"saved"}}`,
	})
	writeTranscript(t, claudeHome, "/other", "22222222-2222-4222-8222-222222222222", []string{
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/other","message":{"content":"other"}}`,
	})

	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.sessions[sessionID] = &Session{
		id:                    sessionID,
		cwd:                   "/repo",
		additionalDirectories: []string{"/shared"},
	}

	cwd := "/repo"
	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{
		Cwd: &cwd,
	})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, acp.SessionId(sessionID), list.Sessions[0].SessionId)
	require.Equal(t, []string{"/shared"}, list.Sessions[0].AdditionalDirectories)

	list, err = agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 2)

	otherCwd := "/missing"
	list, err = agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &otherCwd})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cursor: acp.Ptr("bad")})
	require.Error(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = agent.ListSessions(ctx, acp.ListSessionsRequest{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAgentResumeAndLoadSession(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
		`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:01Z","message":{"content":[{"type":"text","text":"hi"}]}}`,
	})

	fake := newAgentFakeTransport()
	fake.setControlErrors(map[string]string{"get_context_usage": "usage failed"})

	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)

		return claude.NewClient(nil, options, fake)
	}

	resume, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	requireNoTopLevelConfigState(t, resume)
	require.NotNil(t, findSelectConfig(resume.ConfigOptions, configMode))
	require.Equal(t, map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: nil}}, resume.Meta)

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	load, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	requireNoTopLevelConfigState(t, load)
	require.NotNil(t, findSelectConfig(load.ConfigOptions, configMode))
	require.Equal(t, map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: nil}}, load.Meta)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 2
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.Len(t, updates, 2)
	require.Equal(t, "hello", updates[0].Update.UserMessageChunk.Content.Text.Text)
	require.Equal(t, "hi", updates[1].Update.AgentMessageChunk.Content.Text.Text)
}

func TestAgentLoadSessionEmitsAvailableCommandsAfterReplay(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
	})

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"commands": []any{
			map[string]any{"name": "debug", "description": "Debug command"},
		},
	}

	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)

		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 2
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.NotNil(t, updates[0].Update.UserMessageChunk)
	require.Equal(t, "hello", updates[0].Update.UserMessageChunk.Content.Text.Text)
	require.NotNil(t, updates[1].Update.AvailableCommandsUpdate)
	require.Equal(t, "debug", updates[1].Update.AvailableCommandsUpdate.AvailableCommands[0].Name)
}

func TestAgentLoadSessionAvailableCommandsUpdateError(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
	})

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"commands": []any{
			map[string]any{"name": "debug", "description": "Debug command"},
		},
	}

	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)

		return claude.NewClient(nil, options, fake)
	}

	updateErr := errors.New("update failed")
	agent.setConnection(&stubAgentClient{updateErr: updateErr, updateErrAfter: 1})

	_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, updateErr)
	require.True(t, fake.isClosed())
	require.Empty(t, agent.sessions)
}

func TestAgentResumeSessionReusesExistingSession(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	fake := newAgentFakeTransport()
	call := 0
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)
		call++

		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fake.isClosed())

	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fake.isClosed())
	require.Equal(t, 1, call)
}

func TestAgentResumeSessionActiveEmitError(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
	start := sessionStart{Cwd: "/repo", McpServers: []acp.McpServer{}}
	session := &Session{
		agent:             agent,
		id:                "session-1",
		fingerprint:       sessionStartFingerprint(start),
		availableCommands: []claude.SlashCommand{{Name: "debug"}},
	}
	agent.sessions[session.id] = session

	_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  session.id,
		Cwd:        start.Cwd,
		McpServers: start.McpServers,
	})
	require.Error(t, err)
}

func TestAgentResumeSessionReplacesExistingSession(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	fakes := []*agentFakeTransport{newAgentFakeTransport(), newAgentFakeTransport()}
	call := 0
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)
		require.Less(t, call, len(fakes))

		fake := fakes[call]
		call++

		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fakes[0].isClosed())

	fakes[0].closeErr = errors.New("close failed")
	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/other",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.True(t, fakes[0].isClosed())
	require.False(t, fakes[1].isClosed())
	require.Equal(t, 2, call)
}

func TestAgentLoadSessionReusesExistingSession(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
	})

	fake := newAgentFakeTransport()
	call := 0
	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)
		call++

		return claude.NewClient(nil, options, fake)
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})

	_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fake.isClosed())

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fake.isClosed())
	require.Equal(t, 1, call)
}

func TestAgentLoadSessionReplacesExistingSession(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
	})

	fakes := []*agentFakeTransport{newAgentFakeTransport(), newAgentFakeTransport()}
	call := 0
	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)
		require.Less(t, call, len(fakes))

		fake := fakes[call]
		call++

		return claude.NewClient(nil, options, fake)
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})

	_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.False(t, fakes[0].isClosed())

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId: sessionID,
		Cwd:       "/repo",
		McpServers: []acp.McpServer{
			{Stdio: &acp.McpServerStdio{Name: "tools", Command: "tool-server"}},
		},
	})
	require.NoError(t, err)
	require.True(t, fakes[0].isClosed())
	require.False(t, fakes[1].isClosed())
	require.Equal(t, 2, call)
}

func TestAgentResumeLoadAndForkErrors(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	startErr := errors.New("start failed")
	fake := newAgentFakeTransport()
	fake.startErr = startErr

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, startErr)

	missingResumeFake := newAgentFakeTransport()
	missingResumeFake.startErr = fmt.Errorf("%w: remote session missing", claude.ErrSessionNotFound)
	missingResumeAgent := NewAgent(WithClaudeHome(t.TempDir()))
	missingResumeAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, missingResumeFake)
	}
	_, err = missingResumeAgent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32002, reqErr.Code)

	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId: sessionID,
		Cwd:       "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})
	path := filepath.Join(claudeHome, "projects", sanitizeTestProjectPath("/repo"), sessionID+".jsonl")

	loadAgent := NewAgent(WithClaudeHome(claudeHome))
	loadAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.NoError(t, os.Remove(path))

		return claude.NewClient(nil, options, newAgentFakeTransport())
	}

	_, err = loadAgent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)

	loadStartErrAgent := NewAgent(WithClaudeHome(claudeHome))
	loadStartErrFake := newAgentFakeTransport()
	loadStartErrFake.startErr = startErr
	loadStartErrAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, loadStartErrFake)
	}
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})
	_, err = loadStartErrAgent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, startErr)

	missingLoadAgent := NewAgent(WithClaudeHome(claudeHome))
	missingLoadFake := newAgentFakeTransport()
	missingLoadFake.startErr = fmt.Errorf("%w: remote query closed", claude.ErrQueryClosed)
	missingLoadAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, missingLoadFake)
	}
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})
	_, err = missingLoadAgent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32002, reqErr.Code)

	_, err = loadAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId: "source-session",
		Cwd:       "/repo",
		McpServers: []acp.UnstableMcpServer{
			{Acp: &acp.UnstableMcpServerAcpInline{Name: "acp", Id: "id"}},
		},
	})
	require.Error(t, err)

	_, err = loadAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{{}},
	})
	require.ErrorAs(t, err, &reqErr)

	forkAgent := NewAgent(WithClaudeHome(t.TempDir()))
	forkFake := newAgentFakeTransport()
	forkFake.startErr = startErr
	forkAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, forkFake)
	}

	_, err = forkAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.ErrorIs(t, err, startErr)
}

func TestAgentForkSession(t *testing.T) {
	t.Parallel()

	claudeHome := t.TempDir()
	store := permissions.Store{ClaudeHome: claudeHome}
	require.NoError(t, store.Save(context.Background(), "source-session", map[string]string{"Read": claude.BehaviorAllow}))

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(claudeHome), WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, "source-session", options.ResumeID)
		require.True(t, options.ForkSession)
		require.Equal(t, "/repo", options.Cwd)
		require.Contains(t, options.MCPConfigJSON, "fork-mcp")

		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:             "source-session",
		Cwd:                   "/repo",
		AdditionalDirectories: []string{"/shared"},
		McpServers: []acp.UnstableMcpServer{
			{
				Stdio: &acp.McpServerStdio{
					Name:    "fork-mcp",
					Command: "mcp",
					Args:    []string{"--ok"},
				},
			},
		},
	})
	require.NoError(t, err)
	requireNoTopLevelConfigState(t, resp)
	require.NotEmpty(t, resp.SessionId)
	require.NotEqual(t, acp.SessionId("source-session"), resp.SessionId)
	requireClaudeVariantMeta(t, resp.Meta, "claude-test", "", []string{})
	require.NotNil(t, resp.ConfigOptions)
	require.Equal(t, acp.SessionConfigValueId("claude-test"), resp.ConfigOptions[0].Select.CurrentValue)

	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, resp.SessionId, list.Sessions[0].SessionId)
	require.Equal(t, []string{"/shared"}, list.Sessions[0].AdditionalDirectories)

	rules, err := store.Load(context.Background(), string(resp.SessionId))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow}, rules)

	fake.incoming <- permissionRequest("fork-perm-1", "Read")

	var permissionResp claude.ControlResponse
	require.Eventually(t, func() bool {
		return findControlResponse(fake, "fork-perm-1", &permissionResp)
	}, time.Second, 10*time.Millisecond)

	payload, ok := permissionResp.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, payload["behavior"])
}

func TestAgentForkSessionClosesStartedSessionWhenAgentClosesMidStart(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.NoError(t, agent.Close())

		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.ErrorIs(t, err, errAgentClosed)
	require.True(t, fake.isClosed())
}

func TestAgentSessionControls(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = setModeConfig(context.Background(), agent, resp.SessionId, modePlan)
	require.NoError(t, err)

	configResp, err := agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modeDefault),
		},
	})
	require.NoError(t, err)
	modeConfig := findSelectConfig(configResp.ConfigOptions, configMode)
	require.NotNil(t, modeConfig)
	require.Equal(t, acp.SessionConfigValueId(modeDefault), modeConfig.CurrentValue)

	configResp, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configModel,
			Value:     "claude-new",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, configResp.ConfigOptions)
	require.Equal(t, acp.SessionConfigValueId("claude-new"), configResp.ConfigOptions[0].Select.CurrentValue)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configOutputStyle,
			Value:     "Explanatory",
		},
	})
	require.NoError(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configEffort,
			Value:     "low",
		},
	})
	require.NoError(t, err)

	configResp, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: resp.SessionId,
			ConfigId:  configFastMode,
			Value:     true,
		},
	})
	require.NoError(t, err)
	fastConfig := findBooleanConfig(configResp.ConfigOptions, configFastMode)
	require.NotNil(t, fastConfig)
	require.True(t, fastConfig.CurrentValue)

	_, err = setModelConfig(context.Background(), agent, resp.SessionId, "claude-next")
	require.NoError(t, err)
}

func TestAgentSessionRuntimeControlsWaitForTurn(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	session, err := agent.session(resp.SessionId)
	require.NoError(t, err)

	turn := session.turnQueue()
	turn <- struct{}{}

	controlSent := make(chan string, 1)
	fake.setSendHook(func(payload any) {
		request, ok := payload.(claude.ControlRequest)
		if !ok {
			return
		}

		subtype, _ := request.Request["subtype"].(string)
		if subtype == "set_permission_mode" {
			controlSent <- subtype
		}
	})

	done := make(chan error, 1)
	go func() {
		_, modeErr := setModeConfig(context.Background(), agent, resp.SessionId, modePlan)
		done <- modeErr
	}()

	select {
	case subtype := <-controlSent:
		t.Fatalf("sent %s while a turn was active", subtype)
	case modeErr := <-done:
		t.Fatalf("mode update completed while a turn was active: %v", modeErr)
	case <-time.After(50 * time.Millisecond):
	}

	<-turn

	select {
	case modeErr := <-done:
		require.NoError(t, modeErr)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued mode update")
	}

	select {
	case subtype := <-controlSent:
		require.Equal(t, "set_permission_mode", subtype)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queued control request")
	}
}

func TestAgentSessionRuntimeControlsCancelWhileWaitingForTurn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		subtype string
		run     func(context.Context, *Agent, acp.SessionId) error
	}{
		{
			name:    "config mode",
			subtype: "set_permission_mode",
			run: func(ctx context.Context, agent *Agent, sessionID acp.SessionId) error {
				_, err := setModeConfig(ctx, agent, sessionID, modePlan)

				return err
			},
		},
		{
			name:    "config model",
			subtype: "set_model",
			run: func(ctx context.Context, agent *Agent, sessionID acp.SessionId) error {
				_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
					ValueId: &acp.SetSessionConfigOptionValueId{
						SessionId: sessionID,
						ConfigId:  configModel,
						Value:     "claude-new",
					},
				})

				return err
			},
		},
		{
			name:    "config fast mode",
			subtype: "apply_flag_settings",
			run: func(ctx context.Context, agent *Agent, sessionID acp.SessionId) error {
				_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
					Boolean: &acp.SetSessionConfigOptionBoolean{
						SessionId: sessionID,
						ConfigId:  configFastMode,
						Value:     true,
					},
				})

				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newAgentFakeTransport()
			agent := NewAgent(WithDefaultModel("claude-test"))
			agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
				return claude.NewClient(nil, options, fake)
			}

			resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
			require.NoError(t, err)

			session, err := agent.session(resp.SessionId)
			require.NoError(t, err)

			turn := session.turnQueue()
			turn <- struct{}{}
			t.Cleanup(func() { <-turn })

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err = tc.run(ctx, agent, resp.SessionId)
			require.ErrorIs(t, err, context.Canceled)
			require.Empty(t, sentControlRequests(fake, tc.subtype))
		})
	}
}

func TestAgentSessionControlErrorsFromClaudeClient(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	sendErr := errors.New("send failed")
	fake.setSendErr(sendErr)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configModel,
			Value:     "claude-new",
		},
	})
	require.ErrorIs(t, err, sendErr)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modePlan),
		},
	})
	require.ErrorIs(t, err, sendErr)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configOutputStyle,
			Value:     "Explanatory",
		},
	})
	require.ErrorIs(t, err, sendErr)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configEffort,
			Value:     "low",
		},
	})
	require.ErrorIs(t, err, sendErr)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: resp.SessionId,
			ConfigId:  configFastMode,
			Value:     true,
		},
	})
	require.ErrorIs(t, err, sendErr)

	_, err = setModelConfig(context.Background(), agent, resp.SessionId, "claude-next")
	require.ErrorIs(t, err, sendErr)

	require.ErrorIs(t, agent.Cancel(context.Background(), acp.CancelNotification{SessionId: resp.SessionId}), sendErr)

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: resp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.ErrorIs(t, err, sendErr)
}

func TestAgentSessionControlUpdateErrorsAndMissingSessions(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	attachFailingConnection(agent)

	_, err = setModeConfig(context.Background(), agent, resp.SessionId, modePlan)
	require.Error(t, err)

	fake.setControlErrors(nil)
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configModel,
			Value:     "claude-new",
		},
	})
	require.Error(t, err)

	fake.setControlErrors(nil)
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: resp.SessionId,
			ConfigId:  configFastMode,
			Value:     true,
		},
	})
	require.Error(t, err)

	_, err = setModelConfig(context.Background(), agent, resp.SessionId, "claude-next")
	require.Error(t, err)

	_, err = setModelConfig(context.Background(), agent, "missing", "claude-next")
	require.Error(t, err)
}

func TestAgentModelEffortApplyErrors(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{
				"value":                 "initial",
				"supportedEffortLevels": []any{"medium"},
			},
			map[string]any{
				"value":                 "low-only",
				"supportedEffortLevels": []any{"low"},
			},
		},
	}
	agent := NewAgent(WithDefaultModel("initial"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configEffort,
			Value:     "medium",
		},
	})
	require.NoError(t, err)

	fake.setControlErrors(map[string]string{"apply_flag_settings": "effort failed"})
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configModel,
			Value:     "low-only",
		},
	})
	require.Error(t, err)

	fake.setControlErrors(nil)
	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configEffort,
			Value:     "medium",
		},
	})
	require.NoError(t, err)

	fake.setControlErrors(map[string]string{"apply_flag_settings": "effort failed"})
	_, err = setModelConfig(context.Background(), agent, resp.SessionId, "low-only")
	require.Error(t, err)
}

func TestAgentCloseSessionCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	fake := newAgentFakeTransport()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.closeErr = closeErr
	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: resp.SessionId})
	require.ErrorIs(t, err, closeErr)

	_, err = agent.session(resp.SessionId)
	require.Error(t, err)
}

func TestAgentNewSessionStartError(t *testing.T) {
	t.Parallel()

	startErr := errors.New("start failed")
	fake := newAgentFakeTransport()
	fake.startErr = startErr

	agent := NewAgent()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.ErrorIs(t, err, startErr)
}

func TestAgentNesLifecycle(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.assistantText = `{"suggestions":[{"kind":"edit","id":"s1","uri":"file:///repo/main.go","edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"// generated\n"}]}]}`

	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, "claude-test", options.Model)
		require.Equal(t, string(modePlan), options.PermissionMode)

		return claude.NewClient(nil, options, fake)
	}

	start, err := agent.UnstableStartNes(context.Background(), acp.UnstableStartNesRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, start.SessionId)

	err = agent.UnstableDidOpenDocument(context.Background(), acp.UnstableDidOpenDocumentNotification{
		SessionId:  start.SessionId,
		Uri:        "file:///repo/main.go",
		LanguageId: "go",
		Text:       "package main\n",
		Version:    1,
	})
	require.NoError(t, err)

	suggest, err := agent.UnstableSuggestNes(context.Background(), acp.UnstableSuggestNesRequest{
		SessionId:   start.SessionId,
		Uri:         "file:///repo/main.go",
		TriggerKind: acp.UnstableNesTriggerKindManual,
		Position:    acp.UnstablePosition{Line: 0, Character: 0},
		Version:     1,
	})
	require.NoError(t, err)
	require.Len(t, suggest.Suggestions, 1)
	require.NotNil(t, suggest.Suggestions[0].Edit)
	require.Equal(t, "s1", suggest.Suggestions[0].Edit.Id)

	require.NoError(t, agent.UnstableAcceptNes(context.Background(), acp.UnstableAcceptNesNotification{
		SessionId: start.SessionId,
		Id:        "s1",
	}))

	reason := acp.UnstableNesRejectReasonIgnored
	require.NoError(t, agent.UnstableRejectNes(context.Background(), acp.UnstableRejectNesNotification{
		SessionId: start.SessionId,
		Id:        "s1",
		Reason:    &reason,
	}))

	agent.mu.Lock()
	require.Len(t, agent.nesSessions[start.SessionId].decisions, 2)
	agent.mu.Unlock()

	_, err = agent.UnstableCloseNes(context.Background(), acp.UnstableCloseNesRequest{SessionId: start.SessionId})
	require.NoError(t, err)

	_, err = agent.UnstableSuggestNes(context.Background(), acp.UnstableSuggestNesRequest{
		SessionId:   start.SessionId,
		Uri:         "file:///repo/main.go",
		TriggerKind: acp.UnstableNesTriggerKindManual,
	})
	require.Error(t, err)
}

func TestAgentNesValidationAndUUIDErrors(t *testing.T) {
	agent := NewAgent()

	_, err := agent.UnstableSuggestNes(context.Background(), acp.UnstableSuggestNesRequest{})
	require.Error(t, err)

	random := uuidRandom
	uuidRandom = errReader{err: errors.New("random failed")}
	t.Cleanup(func() {
		uuidRandom = random
	})

	_, err = agent.UnstableStartNes(context.Background(), acp.UnstableStartNesRequest{})
	require.Error(t, err)

	_, err = agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.Error(t, err)
}

func TestAgentDocumentNotificationErrors(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	ctx := context.Background()

	require.Error(t, agent.UnstableDidOpenDocument(ctx, acp.UnstableDidOpenDocumentNotification{}))

	err := agent.UnstableDidOpenDocument(ctx, acp.UnstableDidOpenDocumentNotification{
		SessionId:  "missing",
		Uri:        "file:///repo/main.go",
		LanguageId: "go",
		Text:       "package main\n",
	})
	require.Error(t, err)

	require.Error(t, agent.UnstableDidChangeDocument(ctx, acp.UnstableDidChangeDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
		Version:   1,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{Text: "package main\n"},
		},
	}))
	require.Error(t, agent.UnstableDidFocusDocument(ctx, acp.UnstableDidFocusDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
		Version:   1,
	}))
	require.Error(t, agent.UnstableDidSaveDocument(ctx, acp.UnstableDidSaveDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
	}))
	require.Error(t, agent.UnstableDidCloseDocument(ctx, acp.UnstableDidCloseDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
	}))

	fake := newAgentFakeTransport()
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	session, err := agent.NewSession(ctx, acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	err = agent.UnstableDidChangeDocument(ctx, acp.UnstableDidChangeDocumentNotification{
		SessionId: session.SessionId,
		Uri:       "file:///repo/main.go",
		Version:   1,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 99, Character: 0},
					End:   acp.UnstablePosition{Line: 99, Character: 1},
				},
				Text: "bad",
			},
		},
	})
	require.Error(t, err)
}

func TestAgentDocumentNotificationsHonorCanceledContext(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, agent.UnstableDidOpenDocument(ctx, acp.UnstableDidOpenDocumentNotification{
		SessionId:  "missing",
		Uri:        "file:///repo/main.go",
		LanguageId: "go",
		Text:       "package main\n",
	}), context.Canceled)
	require.ErrorIs(t, agent.UnstableDidChangeDocument(ctx, acp.UnstableDidChangeDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
		Version:   1,
		ContentChanges: []acp.UnstableTextDocumentContentChangeEvent{
			{Text: "package main\n"},
		},
	}), context.Canceled)
	require.ErrorIs(t, agent.UnstableDidFocusDocument(ctx, acp.UnstableDidFocusDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
		Version:   1,
	}), context.Canceled)
	require.ErrorIs(t, agent.UnstableDidSaveDocument(ctx, acp.UnstableDidSaveDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
	}), context.Canceled)
	require.ErrorIs(t, agent.UnstableDidCloseDocument(ctx, acp.UnstableDidCloseDocumentNotification{
		SessionId: "missing",
		Uri:       "file:///repo/main.go",
	}), context.Canceled)
}

func TestAgentErrors(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))

	_, err := agent.Prompt(context.Background(), acp.PromptRequest{SessionId: "missing"})
	require.Error(t, err)

	require.Error(t, agent.Cancel(context.Background(), acp.CancelNotification{SessionId: "missing"}))

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: "missing"})
	require.Error(t, err)

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  "missing",
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32002, reqErr.Code)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = agent.LoadSession(cancelled, acp.LoadSessionRequest{
		SessionId:  "missing",
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.ErrorIs(t, err, context.Canceled)

	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{
		SessionId: "missing",
		Cwd:       "/repo",
		Meta: map[string]any{claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{"bad": true},
		}},
	})
	require.Error(t, err)

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId: "missing",
		Cwd:       "/repo",
		Meta: map[string]any{claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{"bad": true},
		}},
	})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: "missing",
			ConfigId:  configModel,
			Value:     "claude-test",
		},
	})
	require.Error(t, err)

	_, err = agent.NewSession(context.Background(), acp.NewSessionRequest{
		McpServers: []acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}}},
	})
	require.Error(t, err)

	_, err = agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		Meta: map[string]any{claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{"bad": true},
		}},
	})
	require.Error(t, err)

	home := t.TempDir()
	storeDir := filepath.Join(home, "acp-go-claude")
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, "session-permissions.json"), []byte("{bad"), 0o600))

	corruptRulesAgent := NewAgent(WithClaudeHome(home))
	corruptRulesFake := newAgentFakeTransport()
	corruptRulesAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, corruptRulesFake)
	}
	forkResp, err := corruptRulesAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.NoError(t, err)
	_, _ = corruptRulesAgent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: forkResp.SessionId})

	readErrHome := t.TempDir()
	readErrStoreDir := filepath.Join(readErrHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(readErrStoreDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(readErrStoreDir, "session-permissions.json"), 0o700))

	readErrAgent := NewAgent(WithClaudeHome(readErrHome))
	_, err = readErrAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  "source-session",
		Cwd:        "/repo",
		McpServers: []acp.UnstableMcpServer{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "load permission rules")
}

func TestAgentInvalidModeAndConfig(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithDefaultModel("claude-test"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  "unknown",
			Value:     "x",
		},
	})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     "unknown",
		},
	})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: resp.SessionId,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(modeAuto),
		},
	})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: resp.SessionId,
			ConfigId:  "unknown",
			Value:     true,
		},
	})
	require.Error(t, err)

	_, err = agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{
			SessionId: "missing",
			ConfigId:  configFastMode,
			Value:     true,
		},
	})
	require.Error(t, err)
}

func TestPaginateSessionInfos(t *testing.T) {
	t.Parallel()

	sessions := make([]acp.SessionInfo, listSessionsPageSize+1)
	for i := range sessions {
		sessions[i].SessionId = acp.SessionId(fmt.Sprintf("session-%d", i))
	}

	first, cursor, err := paginateSessionInfos(sessions, nil)
	require.NoError(t, err)
	require.Len(t, first, listSessionsPageSize)
	require.NotNil(t, cursor)

	second, nextCursor, err := paginateSessionInfos(sessions, cursor)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Nil(t, nextCursor)

	invalid := "invalid"
	_, _, err = paginateSessionInfos(sessions, &invalid)
	require.Error(t, err)

	_, err = decodeListCursor(acp.Ptr("!!!"))
	require.Error(t, err)

	pastEnd := encodeListCursor(len(sessions) + 1)
	_, _, err = paginateSessionInfos(sessions, &pastEnd)
	require.Error(t, err)
}

func TestAgentLocalHelpers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode acp.SessionModeId
		want string
		ok   bool
	}{
		{modeDefault, "default", true},
		{modePlan, "plan", true},
		{modeAcceptEdits, "acceptEdits", true},
		{modeBypassPermissions, "bypassPermissions", true},
		{modeAuto, "auto", true},
		{modeDontAsk, "dontAsk", true},
		{"unknown", "", false},
	} {
		got, ok := permissionModeForACP(tc.mode)
		require.Equal(t, tc.want, got)
		require.Equal(t, tc.ok, ok)
	}

	modeValues := modeSelectOptions(
		"claude-test",
		[]claude.AvailableModelInfo{{Value: "claude-test", SupportsAutoMode: true}},
	)
	require.NotNil(t, findSelectOption(modeValues, acp.SessionConfigValueId(modeAuto)))
	require.NotNil(t, findSelectOption(modeValues, acp.SessionConfigValueId(modeDontAsk)))

	session := &Session{cwd: "/repo", additionalDirectories: []string{"/shared"}}
	require.True(t, sessionMatchesListFilters(session, acp.ListSessionsRequest{}))

	cwd := "/repo"
	require.True(t, sessionMatchesListFilters(session, acp.ListSessionsRequest{Cwd: &cwd}))

	otherCwd := "/other"
	require.False(t, sessionMatchesListFilters(session, acp.ListSessionsRequest{Cwd: &otherCwd}))

	require.Equal(t, []string{"one"}, stringSliceValue([]string{"one"}))
	require.Nil(t, stringSliceValue(1))
	require.Equal(t, []string{"one"}, answerStrings([]string{"", "one"}))
	require.Nil(t, answerStrings(1))
	require.Equal(t, "Only one?", askUserQuestionMessage([]askUserQuestion{{Question: "Only one?"}}))

	_, parseMessage := parseAskUserQuestions(nil)
	require.NotEmpty(t, parseMessage)
	_, parseMessage = parseAskUserQuestions(map[string]any{askFieldQuestions: []any{"bad"}})
	require.NotEmpty(t, parseMessage)

	questions, parseMessage := parseAskUserQuestions(map[string]any{
		askFieldQuestions: []any{
			map[string]any{
				askFieldQuestion:    "Pick one",
				askFieldMultiSelect: true,
				askFieldOptions: []any{
					"bad",
					map[string]any{"label": "A", askFieldDescription: "Alpha"},
				},
			},
		},
	})
	require.Empty(t, parseMessage)
	require.Len(t, questions, 1)
	require.Equal(t, "question_1", questions[0].ID)
	require.True(t, questions[0].MultiSelect)
	require.Len(t, questions[0].Options, 1)

	_, applyMessage := applyAskUserAnswers(map[string]any{}, questions, nil)
	require.NotEmpty(t, applyMessage)
	_, applyMessage = applyAskUserAnswers(map[string]any{}, questions, map[string]any{"missing": "answer"})
	require.NotEmpty(t, applyMessage)

	updatedInput, applyMessage := applyAskUserAnswers(
		map[string]any{"keep": "value"},
		questions,
		map[string]any{"question_1": []any{"", "A"}},
	)
	require.Empty(t, applyMessage)
	require.Equal(t, map[string]any{"keep": "value", askFieldAnswers: map[string]any{"Pick one": "A"}}, updatedInput)
	require.Equal(t, "question_1", claudeAskAnswerKey(askUserQuestion{ID: "question_1"}))
	require.Equal(t, "A", claudeAskAnswerValue(askUserQuestion{}, []string{"A", "B"}))

	require.Equal(t, "from-raw", elicitationIDFromSystem(&claude.SystemMessage{
		Raw: map[string]any{"elicitation_id": "from-raw"},
	}))
}

func TestAgentExperimentalNoopMethods(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	ctx := context.Background()

	require.Error(t, agent.UnstableAcceptNes(ctx, acp.UnstableAcceptNesNotification{}))
	require.Error(t, agent.UnstableRejectNes(ctx, acp.UnstableRejectNesNotification{}))

	_, err := agent.Logout(ctx, acp.LogoutRequest{})
	require.NoError(t, err)
}

func permissionRequest(requestID string, toolName string) map[string]any {
	return map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request": map[string]any{
			"subtype":     "can_use_tool",
			"tool_name":   toolName,
			"tool_use_id": "tool-1",
			"input":       map[string]any{"file_path": "/tmp/a"},
		},
	}
}

func findControlResponse(fake *agentFakeTransport, requestID string, out *claude.ControlResponse) bool {
	for _, payload := range fake.sentPayloads() {
		resp, ok := payload.(claude.ControlResponse)
		if ok && resp.Response["request_id"] == requestID {
			*out = resp

			return true
		}
	}

	return false
}

type sessionConfigSetter interface {
	SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error)
}

func setModelConfig(
	ctx context.Context,
	setter sessionConfigSetter,
	sessionID acp.SessionId,
	model string,
) (acp.SetSessionConfigOptionResponse, error) {
	return setter.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  configModel,
			Value:     acp.SessionConfigValueId(model),
		},
	})
}

func setModeConfig(
	ctx context.Context,
	setter sessionConfigSetter,
	sessionID acp.SessionId,
	mode acp.SessionModeId,
) (acp.SetSessionConfigOptionResponse, error) {
	return setter.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: sessionID,
			ConfigId:  configMode,
			Value:     acp.SessionConfigValueId(mode),
		},
	})
}

func findBooleanConfig(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionBoolean {
	for _, option := range options {
		if option.Boolean != nil && option.Boolean.Id == id {
			return option.Boolean
		}
	}

	return nil
}

func findSelectConfig(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			return option.Select
		}
	}

	return nil
}

func findSelectOption(options acp.SessionConfigSelectOptionsUngrouped, value acp.SessionConfigValueId) *acp.SessionConfigSelectOption {
	for _, option := range options {
		if option.Value == value {
			return &option
		}
	}

	return nil
}

func requireNoTopLevelConfigState(t *testing.T, response any) {
	t.Helper()

	encoded, err := json.Marshal(response)
	require.NoError(t, err)

	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Contains(t, object, "configOptions")
	require.NotContains(t, object, "models")
	require.NotContains(t, object, "modes")
}

func requireModelOptionBasics(t *testing.T, got acp.SessionConfigSelectOptionsUngrouped, want []acp.SessionConfigSelectOption) {
	t.Helper()

	require.Len(t, got, len(want))
	for i, wantOption := range want {
		require.Equal(t, wantOption.Name, got[i].Name)
		require.Equal(t, wantOption.Value, got[i].Value)
		require.Equal(t, wantOption.Description, got[i].Description)
		require.Equal(t, wantOption.Meta, got[i].Meta)
	}
}

func claudeModelMetaForTest(values map[string]any) map[string]any {
	return map[string]any{claudeMetaKey: values}
}

func requireClaudeVariantMeta(t *testing.T, meta map[string]any, model string, variant string, variants []string) {
	t.Helper()

	claude, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, model, claude[claudeModelMetaModelIDKey])
	if variant == "" {
		require.Nil(t, claude[claudeModelMetaVariantKey])
	} else {
		require.Equal(t, variant, claude[claudeModelMetaVariantKey])
	}
	require.Equal(t, variants, claude[claudeModelMetaAvailableVariantsKey])
}

func sentUserPayloads(fake *agentFakeTransport) []map[string]any {
	var payloads []map[string]any
	for _, payload := range fake.sentPayloads() {
		mapped, ok := payload.(map[string]any)
		if ok && mapped["type"] == "user" {
			payloads = append(payloads, mapped)
		}
	}

	return payloads
}

func sentControlRequests(fake *agentFakeTransport, subtype string) []claude.ControlRequest {
	var requests []claude.ControlRequest
	for _, payload := range fake.sentPayloads() {
		request, ok := payload.(claude.ControlRequest)
		if ok && request.Request["subtype"] == subtype {
			requests = append(requests, request)
		}
	}

	return requests
}

func requireAuthMethodTerminal(
	t *testing.T,
	methods []acp.AuthMethod,
	id string,
) *acp.AuthMethodTerminalInline {
	t.Helper()

	method := findAuthMethodTerminal(methods, id)
	require.NotNil(t, method)

	return method
}

func findAuthMethodTerminal(methods []acp.AuthMethod, id string) *acp.AuthMethodTerminalInline {
	for _, method := range methods {
		if method.Terminal != nil && method.Terminal.Id == id {
			return method.Terminal
		}
	}

	return nil
}

func requireAuthMethodAgent(t *testing.T, methods []acp.AuthMethod, id string) *acp.AuthMethodAgent {
	t.Helper()

	for _, method := range methods {
		if method.Agent != nil && method.Agent.Id == id {
			return method.Agent
		}
	}

	t.Fatalf("missing agent auth method %q", id)

	return nil
}

type promptResult struct {
	resp acp.PromptResponse
	err  error
}

func newQueueingAgentForTest(t *testing.T) (*Agent, *agentFakeTransport, *acp.Connection) {
	t.Helper()

	fake := newAgentFakeTransport()
	fake.suppressResult = true

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	rawClient := &rawForwardACPClient{}
	conn := connectAgentRawForTest(t, agent, rawClient.handle)

	return agent, fake, conn
}

func newQueueingSessionForTest(t *testing.T, conn *acp.Connection) acp.NewSessionResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	session, err := acp.SendRequest[acp.NewSessionResponse](conn, ctx, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	return session
}

func promptAsyncRaw(
	conn *acp.Connection,
	ctx context.Context,
	sessionID acp.SessionId,
	text string,
) <-chan promptResult {
	done := make(chan promptResult, 1)
	go func() {
		resp, err := acp.SendRequest[acp.PromptResponse](conn, ctx, acp.AgentMethodSessionPrompt, acp.PromptRequest{
			SessionId: sessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
		})
		done <- promptResult{resp: resp, err: err}
	}()

	return done
}

func requirePromptResult(t *testing.T, done <-chan promptResult) promptResult {
	t.Helper()

	select {
	case result := <-done:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prompt result")

		return promptResult{}
	}
}

func writeTranscript(t *testing.T, claudeHome string, cwd string, sessionID string, lines []string) {
	t.Helper()

	projectDir := filepath.Join(claudeHome, "projects", sanitizeTestProjectPath(cwd))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}

	require.NoError(t, os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(content), 0o600))
}

func sanitizeTestProjectPath(path string) string {
	result := make([]rune, 0, len(path))
	for _, char := range path {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			result = append(result, char)
		} else {
			result = append(result, '-')
		}
	}

	if len(result) == 0 {
		return "-"
	}

	return string(result)
}
