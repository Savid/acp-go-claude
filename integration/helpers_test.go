//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const livePromptRefusalRetries = 1

var integrationLogger = slog.New(slog.DiscardHandler)

func TestMain(m *testing.M) {
	previousLogger := slog.Default()
	slog.SetDefault(integrationLogger)

	code := m.Run()

	slog.SetDefault(previousLogger)
	os.Exit(code)
}

type recordingClient struct {
	mu sync.Mutex

	textChunks             []string
	commands               []acp.AvailableCommand
	usageUpdates           []acp.SessionUsageUpdate
	updates                []acp.SessionUpdate
	permissions            []acp.RequestPermissionRequest
	permission             acp.PermissionOptionId
	elicitations           []acp.UnstableCreateElicitationRequest
	elicitationCompletions []acp.UnstableCompleteElicitationNotification
	elicitationResponse    acp.UnstableCreateElicitationResponse
	extensions             []recordedExtension
}

var _ acp.Client = (*recordingClient)(nil)

var _ interface {
	UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error
	UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
	acp.ExtensionMethodHandler
} = (*recordingClient)(nil)

type recordedExtension struct {
	Method string
	Params map[string]any
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func (c *recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{Content: ""}, nil
}

func (c *recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (c *recordingClient) RequestPermission(
	_ context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, params)
	selected := c.permission
	c.mu.Unlock()

	if selected != "" {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(selected)}, nil
	}

	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce || option.Kind == acp.PermissionOptionKindAllowAlways {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (c *recordingClient) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	var text string

	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, params.Update)

	switch {
	case params.Update.AvailableCommandsUpdate != nil:
		c.commands = append(c.commands, params.Update.AvailableCommandsUpdate.AvailableCommands...)

		return nil
	case params.Update.UsageUpdate != nil:
		c.usageUpdates = append(c.usageUpdates, *params.Update.UsageUpdate)

		return nil
	case params.Update.AgentMessageChunk != nil && params.Update.AgentMessageChunk.Content.Text != nil:
		text = params.Update.AgentMessageChunk.Content.Text.Text
	case params.Update.UserMessageChunk != nil && params.Update.UserMessageChunk.Content.Text != nil:
		text = params.Update.UserMessageChunk.Content.Text.Text
	default:
		return nil
	}

	c.textChunks = append(c.textChunks, text)

	return nil
}

func (c *recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (c *recordingClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (c *recordingClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (c *recordingClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *recordingClient) WaitForTerminalExit(
	context.Context,
	acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (c *recordingClient) UnstableCreateElicitation(
	_ context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitations = append(c.elicitations, params)
	if c.elicitationResponse.Accept != nil ||
		c.elicitationResponse.Decline != nil ||
		c.elicitationResponse.Cancel != nil {
		return c.elicitationResponse, nil
	}

	content := map[string]any{}
	if params.Form != nil {
		for _, required := range params.Form.RequestedSchema.Required {
			content[required] = "Go"
		}
	}
	if len(content) == 0 {
		content["question_1"] = "Go"
	}

	return acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: content},
	}, nil
}

func (c *recordingClient) UnstableCompleteElicitation(
	_ context.Context,
	params acp.UnstableCompleteElicitationNotification,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.elicitationCompletions = append(c.elicitationCompletions, params)

	return nil
}

func (c *recordingClient) HandleExtensionMethod(
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

	c.extensions = append(c.extensions, recordedExtension{Method: method, Params: decoded})

	return map[string]any{}, nil
}

func (c *recordingClient) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return strings.Join(c.textChunks, "")
}

func (c *recordingClient) commandCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.commands)
}

func (c *recordingClient) latestUsage() *acp.SessionUsageUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.usageUpdates) == 0 {
		return nil
	}

	usage := c.usageUpdates[len(c.usageUpdates)-1]

	return &usage
}

func (c *recordingClient) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.textChunks = nil
}

func (c *recordingClient) permissionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.permissions)
}

func (c *recordingClient) permissionSnapshot() []acp.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.RequestPermissionRequest(nil), c.permissions...)
}

func (c *recordingClient) elicitationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.elicitations)
}

func (c *recordingClient) updateSnapshot() []acp.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionUpdate(nil), c.updates...)
}

func (c *recordingClient) extensionSnapshot() []recordedExtension {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedExtension(nil), c.extensions...)
}

type blockingPermissionClient struct {
	recordingClient

	permissionRequested chan struct{}
	permissionReturned  chan acp.RequestPermissionResponse
	requestOnce         sync.Once
}

func newBlockingPermissionClient() *blockingPermissionClient {
	return &blockingPermissionClient{
		permissionRequested: make(chan struct{}),
		permissionReturned:  make(chan acp.RequestPermissionResponse, 1),
	}
}

func (c *blockingPermissionClient) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, params)
	c.mu.Unlock()

	c.requestOnce.Do(func() { close(c.permissionRequested) })

	<-ctx.Done()

	resp := acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	}
	c.permissionReturned <- resp

	return resp, nil
}

func (c *recordingClient) resetRecordedOutput() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.textChunks = nil
	c.updates = nil
	c.usageUpdates = nil
}

func integrationClaudePath(t *testing.T) string {
	t.Helper()

	if os.Getenv("ACP_GO_CLAUDE_RUN_INTEGRATION") != "1" {
		t.Skip("set ACP_GO_CLAUDE_RUN_INTEGRATION=1 to run against the local claude CLI")
	}

	claudePath, err := exec.LookPath("claude")
	require.NoError(t, err)

	return claudePath
}

func connectLiveAgent(
	t *testing.T,
	ctx context.Context,
	client acp.Client,
	initReq acp.InitializeRequest,
	opts ...claudeacp.Option,
) *acp.ClientSideConnection {
	t.Helper()

	clientConn := serveLiveAgentForTest(t, ctx, client, opts...)

	if initReq.ProtocolVersion == 0 {
		initReq.ProtocolVersion = acp.ProtocolVersionNumber
	}
	_, err := clientConn.Initialize(ctx, initReq)
	require.NoError(t, err)

	return clientConn
}

func serveLiveAgentForTest(
	t *testing.T,
	ctx context.Context,
	client acp.Client,
	opts ...claudeacp.Option,
) *acp.ClientSideConnection {
	t.Helper()

	pipes := serveLiveAgentRawForTest(t, ctx, opts...)

	return acp.NewClientSideConnection(client, pipes.clientInput, pipes.agentOutput)
}

type liveAgentPipes struct {
	clientInput io.Writer
	agentOutput io.Reader
}

func serveLiveAgentRawForTest(
	t *testing.T,
	ctx context.Context,
	opts ...claudeacp.Option,
) liveAgentPipes {
	t.Helper()

	base := []claudeacp.Option{
		claudeacp.WithClaudePath(integrationClaudePath(t)),
		claudeacp.WithDefaultModel(os.Getenv("ACP_GO_CLAUDE_MODEL")),
		claudeacp.WithInitializeTimeout(30 * time.Second),
		claudeacp.WithLogger(integrationLogger),
	}

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	serveCtx, stopServe := context.WithCancel(ctx)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- claudeacp.Serve(serveCtx, c2aR, a2cW, append(base, opts...)...)
	}()

	t.Cleanup(func() {
		stopServe()
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()

		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("live agent serve returned: %v", err)
			}
		case <-time.After(time.Second):
			t.Log("live agent serve did not stop within cleanup timeout")
		}
	})

	return liveAgentPipes{clientInput: c2aW, agentOutput: a2cR}
}

func serveLiveAgentConnectionForTest(
	t *testing.T,
	ctx context.Context,
	handler acp.MethodHandler,
	opts ...claudeacp.Option,
) *acp.Connection {
	t.Helper()

	pipes := serveLiveAgentRawForTest(t, ctx, opts...)

	return acp.NewConnection(handler, pipes.clientInput, pipes.agentOutput)
}

func initializeLiveAgentForTest(
	t *testing.T,
	ctx context.Context,
	client acp.Client,
	initReq acp.InitializeRequest,
	opts ...claudeacp.Option,
) (*acp.ClientSideConnection, acp.InitializeResponse) {
	t.Helper()

	clientConn := serveLiveAgentForTest(t, ctx, client, opts...)
	if initReq.ProtocolVersion == 0 {
		initReq.ProtocolVersion = acp.ProtocolVersionNumber
	}

	resp, err := clientConn.Initialize(ctx, initReq)
	require.NoError(t, err)

	return clientConn, resp
}

func connectLiveAgentBinary(
	t *testing.T,
	ctx context.Context,
	client acp.Client,
	initReq acp.InitializeRequest,
) *acp.ClientSideConnection {
	t.Helper()

	agentPath := os.Getenv("ACP_GO_CLAUDE_AGENT_BINARY")
	if agentPath == "" {
		t.Skip("set ACP_GO_CLAUDE_AGENT_BINARY to run compiled binary integration coverage")
	}

	args := []string{"-claude", integrationClaudePath(t)}
	if model := os.Getenv("ACP_GO_CLAUDE_MODEL"); model != "" {
		args = append(args, "-model", model)
	}

	cmd := exec.Command(agentPath, args...) // #nosec G204,G702 -- path is the test-built agent binary.
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	var stderr lockedBuffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				t.Logf("compiled agent exited with error: %v; stderr: %s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			err := <-done
			if err != nil && ctx.Err() == nil {
				t.Logf("compiled agent killed during cleanup: %v; stderr: %s", err, stderr.String())
			}
		}
	})

	clientConn := acp.NewClientSideConnection(client, stdin, stdout)
	if initReq.ProtocolVersion == 0 {
		initReq.ProtocolVersion = acp.ProtocolVersionNumber
	}
	_, err = clientConn.Initialize(ctx, initReq)
	require.NoError(t, err, "stderr: %s", stderr.String())

	return clientConn
}

func promptWithRefusalRetry(
	t *testing.T,
	prompt func() (acp.PromptResponse, error),
) acp.PromptResponse {
	t.Helper()

	var resp acp.PromptResponse
	var err error
	for attempt := 0; attempt <= livePromptRefusalRetries; attempt++ {
		resp, err = prompt()
		require.NoError(t, err)
		if resp.StopReason != acp.StopReasonRefusal || attempt == livePromptRefusalRetries {
			return resp
		}

		t.Logf(
			"live Claude refused prompt on attempt %d/%d; retrying once per integration flake budget",
			attempt+1,
			livePromptRefusalRetries+1,
		)
	}

	return resp
}

func findSelectConfig(t *testing.T, options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	t.Helper()

	if option := selectConfig(options, id); option != nil {
		return option
	}

	t.Fatalf("missing config option %q", id)

	return nil
}

func selectConfig(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			return option.Select
		}
	}

	return nil
}

func booleanConfig(options []acp.SessionConfigOption, id acp.SessionConfigId) *acp.SessionConfigOptionBoolean {
	for _, option := range options {
		if option.Boolean != nil && option.Boolean.Id == id {
			return option.Boolean
		}
	}

	return nil
}

func selectConfigValues(option *acp.SessionConfigOptionSelect) []acp.SessionConfigValueId {
	if option == nil {
		return nil
	}

	values := make([]acp.SessionConfigValueId, 0)
	if option.Options.Ungrouped != nil {
		for _, candidate := range *option.Options.Ungrouped {
			values = append(values, candidate.Value)
		}
	}
	if option.Options.Grouped != nil {
		for _, group := range *option.Options.Grouped {
			for _, candidate := range group.Options {
				values = append(values, candidate.Value)
			}
		}
	}

	return values
}
