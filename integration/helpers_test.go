//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const livePromptRefusalRetries = 1
const envRunIntegration = "ACP_GO_CLAUDE_RUN_INTEGRATION"
const envRunLiveTokens = "ACP_GO_CLAUDE_RUN_LIVE_TOKENS" //nolint:gosec // Environment variable name, not a credential value.
const envClaudeHome = "ACP_GO_CLAUDE_HOME"
const envAnthropicAuthToken = "ANTHROPIC_AUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential value.
const envAnthropicAPIKey = "ANTHROPIC_API_KEY"       //nolint:gosec // Environment variable name, not a credential value.

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
	notifications          []acp.SessionNotification
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
	c.notifications = append(c.notifications, params)

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

func (c *recordingClient) elicitationSnapshot() []acp.UnstableCreateElicitationRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableCreateElicitationRequest(nil), c.elicitations...)
}

func (c *recordingClient) updateSnapshot() []acp.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionUpdate(nil), c.updates...)
}

func (c *recordingClient) notificationSnapshot() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionNotification(nil), c.notifications...)
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

	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run against the local claude CLI", envRunIntegration)
	}

	claudePath, err := exec.LookPath("claude")
	require.NoError(t, err)

	return claudePath
}

// requireLiveTokens skips a test that spends model tokens unless the caller
// opted in explicitly. Smoke runs never spend tokens; only
// `make test-integration-live` sets this variable.
func requireLiveTokens(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunLiveTokens) != "1" {
		t.Skipf("set %s=1 to run live tests that spend model tokens", envRunLiveTokens)
	}
}

func integrationClaudeHome(t *testing.T) string {
	t.Helper()

	return os.Getenv(envClaudeHome)
}

func integrationClaudeSourceHome(t *testing.T) (string, bool) {
	t.Helper()

	source := integrationClaudeHome(t)
	if source != "" {
		return source, true
	}

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	return filepath.Join(home, ".claude"), false
}

func isolatedClaudeHome(t *testing.T) string {
	t.Helper()

	runtime := isolatedClaudeRuntime(t)

	return runtime.home
}

type isolatedClaudeRuntimeConfig struct {
	home string
	env  map[string]string
}

func isolatedClaudeRuntime(t *testing.T) isolatedClaudeRuntimeConfig {
	t.Helper()

	source, explicitSource := integrationClaudeSourceHome(t)
	processAuth := processClaudeAuthAvailable()
	env := copiedClaudeAuthEnv(t, source)
	if len(env) == 0 && !portableClaudeAuthAvailable(t, source) {
		t.Fatalf(
			"live Claude integration requires portable file/env auth; refusing to launch against the real Claude home. "+
				"Set %s, %s, or provide .credentials.json/settings.json auth in %s",
			envAnthropicAuthToken,
			envAnthropicAPIKey,
			envClaudeHome,
		)
	}

	base, err := filepath.Abs(filepath.Join("..", ".tmp", "integration-claude-home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, 0o700))

	target, err := os.MkdirTemp(base, "home-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(target) })

	if explicitSource || !processAuth {
		for _, name := range []string{".credentials.json", "settings.json"} {
			require.NoError(t, copyClaudeHomeFile(source, target, name))
		}
		require.NoError(t, copyClaudeStateFile(source, target))
		require.NoError(t, copyClaudeHomeDir(source, target, "sessions"))
	}

	return isolatedClaudeRuntimeConfig{
		home: target,
		env:  env,
	}
}

// emptyClaudeRuntime is a Claude config directory nothing was copied into and
// no auth environment reaches. Every other runtime here is seeded with portable
// auth so live turns can run, which makes a login driven against one
// indistinguishable from the credential that was already there.
func emptyClaudeRuntime(t *testing.T) isolatedClaudeRuntimeConfig {
	t.Helper()

	base, err := filepath.Abs(filepath.Join("..", ".tmp", "integration-claude-home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, 0o700))

	target, err := os.MkdirTemp(base, "empty-home-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(target) })

	return isolatedClaudeRuntimeConfig{home: target}
}

// requireClaudeHomeHoldsNoCredential fails unless the config dir answers logged
// out and no environment variable supplies a credential in its place. It is
// what makes a later `authenticated` mean the login under test: `auth status`
// answers for the config dir, so one that already holds a credential answers
// the same whatever value was pasted.
func requireClaudeHomeHoldsNoCredential(t *testing.T, home string) {
	t.Helper()

	require.Empty(t, strings.TrimSpace(os.Getenv(envAnthropicAuthToken)),
		"%s supplies a credential to every child; unset it before driving a login", envAnthropicAuthToken)
	require.Empty(t, strings.TrimSpace(os.Getenv(envAnthropicAPIKey)),
		"%s supplies a credential to every child; unset it before driving a login", envAnthropicAPIKey)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	command := exec.CommandContext(ctx, integrationClaudePath(t), "auth", "status", "--json") // #nosec G204 -- path is the discovered Claude CLI.
	command.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+home)

	output, err := command.Output()
	if err != nil {
		return
	}

	var payload struct {
		LoggedIn bool `json:"loggedIn"`
	}

	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output), &payload))
	require.False(t, payload.LoggedIn, "config dir %s already holds a credential", home)
}

func copyClaudeHomeFile(sourceDir string, targetDir string, name string) error {
	source := filepath.Join(sourceDir, name)
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	data, err = nullClaudeRefreshTokens(data)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(targetDir, name), data, 0o600)
}

func copyClaudeStateFile(sourceDir string, targetDir string) error {
	source := filepath.Join(sourceDir, ".claude.json")
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) && filepath.Base(sourceDir) == ".claude" {
		source = filepath.Join(filepath.Dir(sourceDir), ".claude.json")
	}

	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	data, err = nullClaudeRefreshTokens(data)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(targetDir, ".claude.json"), data, 0o600)
}

func copyClaudeHomeDir(sourceDir string, targetDir string, name string) error {
	sourceRoot := filepath.Join(sourceDir, name)
	info, err := os.Stat(sourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}

	targetRoot := filepath.Join(targetDir, name)
	return filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetRoot, rel)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeType != 0 {
			return nil
		}

		data, err := os.ReadFile(path) // #nosec G304 -- integration helper copies selected local Claude home files.
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			data, err = nullClaudeRefreshTokens(data)
			if err != nil {
				return err
			}
		}

		return os.WriteFile(target, data, 0o600) // #nosec G306 -- private integration temp home.
	})
}

func nullClaudeRefreshTokens(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}

	nullRefreshTokens(value)

	out, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	return append(out, '\n'), nil
}

func nullRefreshTokens(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "refreshToken" {
				typed[key] = nil
				continue
			}
			nullRefreshTokens(child)
		}
	case []any:
		for _, child := range typed {
			nullRefreshTokens(child)
		}
	}
}

func copiedClaudeAuthEnv(t *testing.T, sourceDir string) map[string]string {
	t.Helper()

	if processClaudeAuthAvailable() {
		return nil
	}

	data, err := os.ReadFile(filepath.Join(sourceDir, ".credentials.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)

	var value any
	require.NoError(t, json.Unmarshal(data, &value))

	token := strings.TrimSpace(claudeAccessToken(value))
	if token == "" {
		return nil
	}

	return map[string]string{envAnthropicAuthToken: token}
}

func portableClaudeAuthAvailable(t *testing.T, sourceDir string) bool {
	t.Helper()

	if processClaudeAuthAvailable() {
		return true
	}

	if token := strings.TrimSpace(claudeAccessTokenFromFile(t, filepath.Join(sourceDir, ".credentials.json"))); token != "" {
		return true
	}

	return claudeSettingsAuthAvailable(t, filepath.Join(sourceDir, "settings.json"))
}

func processClaudeAuthAvailable() bool {
	return strings.TrimSpace(os.Getenv(envAnthropicAuthToken)) != "" ||
		strings.TrimSpace(os.Getenv(envAnthropicAPIKey)) != ""
}

func claudeAccessTokenFromFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)

	var value any
	require.NoError(t, json.Unmarshal(data, &value))

	return claudeAccessToken(value)
}

func claudeSettingsAuthAvailable(t *testing.T, path string) bool {
	t.Helper()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	env, _ := raw["env"].(map[string]any)
	return strings.TrimSpace(stringValue(env[envAnthropicAuthToken])) != "" ||
		strings.TrimSpace(stringValue(env[envAnthropicAPIKey])) != ""
}

func stringValue(value any) string {
	text, _ := value.(string)

	return text
}

func claudeAccessToken(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if oauth, ok := typed["claudeAiOauth"].(map[string]any); ok {
			if token, ok := oauth["accessToken"].(string); ok {
				return token
			}
		}
		for _, child := range typed {
			if token := claudeAccessToken(child); token != "" {
				return token
			}
		}
	case []any:
		for _, child := range typed {
			if token := claudeAccessToken(child); token != "" {
				return token
			}
		}
	}

	return ""
}

func mergeClaudeEnv(env map[string]string) claudeacp.Option {
	return func(options *claudeacp.Options) {
		if len(env) == 0 {
			return
		}
		if options.Env == nil {
			options.Env = map[string]string{}
		}
		for key, value := range env {
			if strings.TrimSpace(options.Env[key]) == "" {
				options.Env[key] = value
			}
		}
	}
}

func mergedProcessEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	processEnv := os.Environ()
	seen := make(map[string]struct{}, len(processEnv))
	for _, item := range processEnv {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			seen[key] = struct{}{}
		}
	}

	for key, value := range env {
		if _, ok := seen[key]; ok {
			continue
		}
		processEnv = append(processEnv, key+"="+value)
	}

	return processEnv
}

func permissionGateOptions() []claudeacp.Option {
	return []claudeacp.Option{
		claudeacp.WithClaudeDefaultPermissionMode("default"),
		claudeacp.WithClaudeSettingSources(),
	}
}

func requirePortableClaudeAuth(t *testing.T) {
	t.Helper()

	source, _ := integrationClaudeSourceHome(t)
	if !portableClaudeAuthAvailable(t, source) {
		t.Skip("store-backed materialized resume requires portable Claude file/env auth")
	}
}

func parallelWhenPortableClaudeAuth(t *testing.T) {
	t.Helper()

	source, _ := integrationClaudeSourceHome(t)
	if portableClaudeAuthAvailable(t, source) {
		t.Parallel()
	}
}

// integrationContainmentOption opts the in-process agent into Darwin
// containment, which the agent refuses to launch without. Elsewhere the option
// is rejected, so the tier supplies it only on darwin.
func integrationContainmentOption() claudeacp.Option {
	if runtime.GOOS == "darwin" {
		return claudeacp.WithDarwinBestEffortContainment()
	}

	return func(*claudeacp.Options) {}
}

// integrationContainmentArgs is the same opt-in on the other side of the
// process boundary, where the compiled binary takes it as a flag.
func integrationContainmentArgs() []string {
	if runtime.GOOS == "darwin" {
		return []string{"-darwin-best-effort-containment"}
	}

	return nil
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

	return serveLiveAgentInRuntimeForTest(t, ctx, isolatedClaudeRuntime(t), opts...)
}

func serveLiveAgentInRuntimeForTest(
	t *testing.T,
	ctx context.Context,
	runtime isolatedClaudeRuntimeConfig,
	opts ...claudeacp.Option,
) liveAgentPipes {
	t.Helper()

	claudePath := integrationClaudePath(t)
	base := []claudeacp.Option{
		claudeacp.WithExecutablePath(claudePath),
		claudeacp.WithHome(runtime.home),
		claudeacp.WithDefaultModel(os.Getenv("ACP_GO_CLAUDE_MODEL")),
		claudeacp.WithClaudeInitializeTimeout(30 * time.Second),
		claudeacp.WithLogger(integrationLogger),
		integrationContainmentOption(),
	}

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	serveCtx, stopServe := context.WithCancel(ctx)

	serveErr := make(chan error, 1)
	go func() {
		options := append(base, opts...)
		options = append(options, mergeClaudeEnv(runtime.env))
		serveErr <- claudeacp.Serve(serveCtx, c2aR, a2cW, options...)
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

	claudePath := integrationClaudePath(t)
	runtime := isolatedClaudeRuntime(t)
	args := integrationContainmentArgs()
	args = append(args, "-path", claudePath, "-home", runtime.home)
	if model := os.Getenv("ACP_GO_CLAUDE_MODEL"); model != "" {
		args = append(args, "-model", model)
	}

	cmd := exec.Command(agentPath, args...) // #nosec G204,G702 -- path is the test-built agent binary.
	cmd.Env = mergedProcessEnv(runtime.env)
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

func selectConfigValueAvailable(option *acp.SessionConfigOptionSelect, value acp.SessionConfigValueId) bool {
	for _, candidate := range selectConfigValues(option) {
		if candidate == value {
			return true
		}
	}

	return false
}

func configUpdateSelect(update acp.SessionUpdate, id acp.SessionConfigId) *acp.SessionConfigOptionSelect {
	if update.ConfigOptionUpdate == nil {
		return nil
	}

	return selectConfig(update.ConfigOptionUpdate.ConfigOptions, id)
}
