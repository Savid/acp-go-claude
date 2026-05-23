package claudeacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type stubAgentClient struct {
	mu             sync.Mutex
	connectErr     error
	disconnectErr  error
	notifyErr      error
	permission     acp.PermissionOptionId
	permissionErr  error
	updateErr      error
	updateErrAfter int
	updateCount    int
	updates        []acp.SessionNotification
	messageResp    acp.UnstableMessageMcpResponse
	messageErr     error
	messageHook    func(context.Context)
	extensionErr   error
}

type closeBridgeOnConnectClient struct {
	stubAgentClient

	onConnect    func()
	disconnected chan struct{}
	once         sync.Once
}

type cancelOnMarshal struct {
	cancel context.CancelFunc
}

func (c cancelOnMarshal) MarshalJSON() ([]byte, error) {
	c.cancel()

	return []byte(`"cancelled"`), nil
}

type failingMCPTokenFile struct {
	name     string
	writeErr error
	syncErr  error
	closeErr error
}

func (f failingMCPTokenFile) Name() string { return f.name }

func (f failingMCPTokenFile) WriteString(string) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	return 0, nil
}

func (f failingMCPTokenFile) Close() error {
	return f.closeErr
}

func (f failingMCPTokenFile) Sync() error {
	return f.syncErr
}

func (c *closeBridgeOnConnectClient) UnstableConnectMcp(
	context.Context,
	acp.UnstableConnectMcpRequest,
) (acp.UnstableConnectMcpResponse, error) {
	if c.onConnect != nil {
		c.onConnect()
	}

	return acp.UnstableConnectMcpResponse{ConnectionId: "conn-1"}, nil
}

func (c *closeBridgeOnConnectClient) UnstableDisconnectMcp(
	context.Context,
	acp.UnstableDisconnectMcpRequest,
) (acp.UnstableDisconnectMcpResponse, error) {
	c.once.Do(func() {
		close(c.disconnected)
	})

	return acp.UnstableDisconnectMcpResponse{}, nil
}

func (c *stubAgentClient) Done() <-chan struct{} { return make(chan struct{}) }

func (c *stubAgentClient) SetLogger(*slog.Logger) {}

func (c *stubAgentClient) UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error {
	return nil
}

func (c *stubAgentClient) UnstableCreateElicitation(
	context.Context,
	acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return acp.UnstableCreateElicitationResponse{}, nil
}

func (c *stubAgentClient) CreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	_ elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.UnstableCreateElicitation(ctx, params)
}

func (c *stubAgentClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}

func (c *stubAgentClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (c *stubAgentClient) UnstableConnectMcp(
	context.Context,
	acp.UnstableConnectMcpRequest,
) (acp.UnstableConnectMcpResponse, error) {
	c.mu.Lock()
	connectErr := c.connectErr
	c.mu.Unlock()

	if connectErr != nil {
		return acp.UnstableConnectMcpResponse{}, connectErr
	}

	return acp.UnstableConnectMcpResponse{ConnectionId: "conn-1"}, nil
}

func (c *stubAgentClient) UnstableDisconnectMcp(
	context.Context,
	acp.UnstableDisconnectMcpRequest,
) (acp.UnstableDisconnectMcpResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return acp.UnstableDisconnectMcpResponse{}, c.disconnectErr
}

func (c *stubAgentClient) UnstableMessageMcp(
	ctx context.Context,
	_ acp.UnstableMessageMcpRequest,
) (acp.UnstableMessageMcpResponse, error) {
	c.mu.Lock()
	messageHook := c.messageHook
	messageErr := c.messageErr
	messageResp := c.messageResp
	c.mu.Unlock()

	if messageHook != nil {
		messageHook(ctx)
	}
	if messageErr != nil {
		return nil, messageErr
	}
	if messageResp != nil {
		return messageResp, nil
	}

	return map[string]any{"ok": true}, nil
}

func (c *stubAgentClient) UnstableNotifyMcp(context.Context, acp.UnstableMessageMcpNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.notifyErr
}

func (c *stubAgentClient) RequestPermission(
	context.Context,
	acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	permissionErr := c.permissionErr
	permission := c.permission
	c.mu.Unlock()

	if permissionErr != nil {
		return acp.RequestPermissionResponse{}, permissionErr
	}

	if permission == "" {
		return acp.RequestPermissionResponse{}, nil
	}

	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected(permission),
	}, nil
}

func (c *stubAgentClient) SessionUpdate(_ context.Context, update acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.updateErr != nil && (c.updateErrAfter == 0 || c.updateCount >= c.updateErrAfter) {
		return c.updateErr
	}

	c.updateCount++
	c.updates = append(c.updates, update)

	return nil
}

func (c *stubAgentClient) recordedUpdates() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.SessionNotification(nil), c.updates...)
}

func (c *stubAgentClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, nil
}

func (c *stubAgentClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (c *stubAgentClient) TerminalOutput(
	context.Context,
	acp.TerminalOutputRequest,
) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (c *stubAgentClient) ReleaseTerminal(
	context.Context,
	acp.ReleaseTerminalRequest,
) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *stubAgentClient) WaitForTerminalExit(
	context.Context,
	acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (c *stubAgentClient) NotifyExtension(context.Context, string, any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.extensionErr
}

type rawMCPACPClient struct {
	mu sync.Mutex

	connects      []acp.UnstableConnectMcpRequest
	requests      []acp.UnstableMessageMcpRequest
	notifications []acp.UnstableMessageMcpRequest
	disconnects   []acp.UnstableDisconnectMcpRequest
}

func (c *rawMCPACPClient) handle(
	_ context.Context,
	method string,
	params json.RawMessage,
) (any, *acp.RequestError) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch method {
	case acp.ClientMethodMcpConnect:
		var request acp.UnstableConnectMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.connects = append(c.connects, request)

		return acp.UnstableConnectMcpResponse{ConnectionId: "conn-1"}, nil
	case acp.ClientMethodMcpMessage:
		var request acp.UnstableMessageMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		if isMCPNotificationMethod(request.Method) {
			c.notifications = append(c.notifications, request)

			return nil, nil
		}

		c.requests = append(c.requests, request)

		return map[string]any{"ok": true, "method": request.Method}, nil
	case acp.ClientMethodMcpDisconnect:
		var request acp.UnstableDisconnectMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.disconnects = append(c.disconnects, request)

		return acp.UnstableDisconnectMcpResponse{}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func (c *rawMCPACPClient) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.connects) > 0
}

func (c *rawMCPACPClient) recordedRequests() []acp.UnstableMessageMcpRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableMessageMcpRequest(nil), c.requests...)
}

func (c *rawMCPACPClient) recordedNotifications() []acp.UnstableMessageMcpRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.UnstableMessageMcpRequest(nil), c.notifications...)
}

func (c *rawMCPACPClient) disconnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.disconnects) > 0
}

func connectAgentRawForTest(
	t *testing.T,
	agent *Agent,
	handler acp.MethodHandler,
) *acp.Connection {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	clientConn := acp.NewConnection(handler, c2aW, a2cR)
	agentConn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setConnection(agentConn)

	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	_, err := acp.SendRequest[acp.InitializeResponse](
		clientConn,
		context.Background(),
		acp.AgentMethodInitialize,
		acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber},
	)
	require.NoError(t, err)

	return clientConn
}

func connectMCPProxyForTest(
	t *testing.T,
	bridge *mcpSessionBridge,
	acpID string,
) (net.Conn, *json.Encoder, *json.Decoder) {
	t.Helper()

	conn, err := net.Dial(bridge.ln.Addr().Network(), bridge.ln.Addr().String())
	require.NoError(t, err)

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	require.NoError(t, enc.Encode(mcpProxyHello{Version: mcpProxyVersion, Token: bridge.token, ACPID: acpID}))

	t.Cleanup(func() { _ = conn.Close() })

	return conn, enc, dec
}

func TestMCPBridgePreparationAndStartErrors(t *testing.T) {
	ctx := context.Background()

	agent := NewAgent(WithMCPProxyCommand("proxy"))
	_, _, err := agent.prepareMCPServers(ctx, "session-1", []acp.McpServer{
		{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
	})
	require.Error(t, err)

	agent.setConnection(&stubAgentClient{})
	stdio := acp.McpServer{Stdio: &acp.McpServerStdio{Name: "fs", Command: "fs"}}
	servers, bridge, err := agent.prepareMCPServers(ctx, "session-1", []acp.McpServer{
		stdio,
		{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	require.Equal(t, "fs", servers[0].Stdio.Name)
	require.Equal(t, "ide", servers[1].Stdio.Name)

	_, err = agent.startSession(ctx, "session-2", sessionStart{
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
			{},
		},
	})
	require.Error(t, err)

	startErr := errors.New("start failed")
	startAgent := NewAgent(WithMCPProxyCommand("proxy"))
	startAgent.setConnection(&stubAgentClient{})
	startAgent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		fake := newAgentFakeTransport()
		fake.startErr = startErr

		return claude.NewClient(nil, options, fake)
	}
	_, err = startAgent.NewSession(ctx, acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
		},
	})
	require.ErrorIs(t, err, startErr)
}

func TestMCPBridgeConstructionErrors(t *testing.T) {
	originalExecutable := currentExecutable
	originalRandReader := mcpRandReader
	originalListen := mcpNetListen
	originalCreateTemp := mcpCreateTemp
	t.Cleanup(func() {
		currentExecutable = originalExecutable
		mcpRandReader = originalRandReader
		mcpNetListen = originalListen
		mcpCreateTemp = originalCreateTemp
	})

	agent := NewAgent()
	currentExecutable = func() (string, error) {
		return "", errors.New("executable failed")
	}
	_, err := agent.newMCPSessionBridge(context.Background(), "session-1")
	require.Error(t, err)
	agent.setConnection(&stubAgentClient{})
	_, _, err = agent.prepareMCPServers(context.Background(), "session-1", []acp.McpServer{
		{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
	})
	require.Error(t, err)

	currentExecutable = func() (string, error) {
		return "proxy", nil
	}
	mcpRandReader = errReader{err: errors.New("random failed")}
	_, err = newMCPProxyToken()
	require.Error(t, err)
	_, err = agent.newMCPSessionBridge(context.Background(), "session-1")
	require.Error(t, err)

	mcpRandReader = strings.NewReader(strings.Repeat("a", mcpProxyTokenBytes))
	mcpCreateTemp = func(string, string) (mcpTokenTempFile, error) {
		return nil, errors.New("create failed")
	}
	_, err = agent.newMCPSessionBridge(context.Background(), "session-1")
	require.Error(t, err)

	mcpRandReader = strings.NewReader(strings.Repeat("a", mcpProxyTokenBytes))
	var tokenFile string
	mcpCreateTemp = func(dir string, pattern string) (mcpTokenTempFile, error) {
		file, createErr := originalCreateTemp(dir, pattern)
		if createErr == nil {
			tokenFile = file.Name()
		}

		return file, createErr
	}
	mcpNetListen = func(string, string) (net.Listener, error) {
		return nil, errors.New("listen failed")
	}
	_, err = agent.newMCPSessionBridge(context.Background(), "session-1")
	require.Error(t, err)
	require.NotEmpty(t, tokenFile)
	_, err = os.Stat(tokenFile)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMCPBridgeTokenFile(t *testing.T) {
	path, err := writeMCPProxyTokenFile("secret")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Remove(path)
	})

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "secret", string(data))
}

func TestMCPBridgeTokenFileErrors(t *testing.T) {
	originalCreateTemp := mcpCreateTemp
	t.Cleanup(func() {
		mcpCreateTemp = originalCreateTemp
	})

	mcpCreateTemp = func(string, string) (mcpTokenTempFile, error) {
		return nil, errors.New("create failed")
	}
	_, err := writeMCPProxyTokenFile("secret")
	require.Error(t, err)

	mcpCreateTemp = func(string, string) (mcpTokenTempFile, error) {
		return failingMCPTokenFile{name: filepath.Join(t.TempDir(), "token"), writeErr: errors.New("write failed")}, nil
	}
	_, err = writeMCPProxyTokenFile("secret")
	require.Error(t, err)

	mcpCreateTemp = func(string, string) (mcpTokenTempFile, error) {
		return failingMCPTokenFile{name: filepath.Join(t.TempDir(), "token"), closeErr: errors.New("close failed")}, nil
	}
	_, err = writeMCPProxyTokenFile("secret")
	require.Error(t, err)

	mcpCreateTemp = func(string, string) (mcpTokenTempFile, error) {
		return failingMCPTokenFile{name: filepath.Join(t.TempDir(), "token"), syncErr: errors.New("sync failed")}, nil
	}
	_, err = writeMCPProxyTokenFile("secret")
	require.Error(t, err)
}

func TestAgentACPTransportMCPConfig(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	var mcpConfig string
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithMCPProxyCommand("proxy-bin", "base-arg"))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		mcpConfig = options.MCPConfigJSON

		return claude.NewClient(nil, options, fake)
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})

	session, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd: "/repo",
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: session.SessionId})
	})

	var decoded map[string]map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(mcpConfig), &decoded))

	server := decoded["mcpServers"]["ide"]
	require.Equal(t, "stdio", server["type"])
	require.Equal(t, "proxy-bin", server["command"])
	env, ok := server["env"].(map[string]any)
	require.True(t, ok)
	tokenFile, ok := env[MCPProxyTokenFileEnv].(string)
	require.True(t, ok)
	require.NotEmpty(t, tokenFile)
	token, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	require.NotContains(t, mcpConfig, string(token))

	args, ok := server["args"].([]any)
	require.True(t, ok)
	require.Equal(t, "base-arg", args[0])
	require.Equal(t, mcpProxySubcommand, args[1])
	require.NotContains(t, args, "-token")
	require.Contains(t, args, "-acp-id")
	require.Contains(t, args, "ide-1")
}

func TestMCPBridgeForwardsProxyMessagesToACP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &rawMCPACPClient{}
	_ = connectAgentRawForTest(t, agent, client.handle)

	bridge, err := agent.newMCPSessionBridge(ctx, "session-1", map[string]struct{}{"server-1": {}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })

	proxy, enc, dec := connectMCPProxyForTest(t, bridge, "server-1")
	require.NotNil(t, proxy)

	requestID := json.RawMessage(`"req-1"`)
	require.NoError(t, enc.Encode(mcpRPCMessage{
		JSONRPC: mcpJSONRPCVersion,
		ID:      &requestID,
		Method:  "tools/list",
		Params:  json.RawMessage(`{"cursor":"a"}`),
	}))

	var response mcpRPCMessage
	require.NoError(t, dec.Decode(&response))
	require.Equal(t, requestID, *response.ID)
	require.JSONEq(t, `{"method":"tools/list","ok":true}`, string(response.Result))

	require.Eventually(t, client.connected, time.Second, 10*time.Millisecond)
	requests := client.recordedRequests()
	require.Len(t, requests, 1)
	require.Equal(t, acp.UnstableMcpConnectionId("conn-1"), requests[0].ConnectionId)
	require.Equal(t, "tools/list", requests[0].Method)
	require.Equal(t, "a", requests[0].Params["cursor"])

	require.NoError(t, enc.Encode(mcpRPCMessage{
		JSONRPC: mcpJSONRPCVersion,
		Method:  "notifications/progress",
		Params:  json.RawMessage(`{"progress":1}`),
	}))
	require.Eventually(t, func() bool {
		return len(client.recordedNotifications()) == 1
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, proxy.Close())
	require.Eventually(t, client.disconnected, time.Second, 10*time.Millisecond)
}

func TestMCPBridgeOutlivesSetupContext(t *testing.T) {
	t.Parallel()

	setupCtx, cancelSetup := context.WithCancel(context.Background())

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &rawMCPACPClient{}
	_ = connectAgentRawForTest(t, agent, client.handle)

	bridge, err := agent.newMCPSessionBridge(setupCtx, "session-1", map[string]struct{}{"server-1": {}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })

	cancelSetup()

	proxy, enc, dec := connectMCPProxyForTest(t, bridge, "server-1")
	require.NoError(t, proxy.SetDeadline(time.Now().Add(time.Second)))

	requestID := json.RawMessage(`"req-1"`)
	require.NoError(t, enc.Encode(mcpRPCMessage{
		JSONRPC: mcpJSONRPCVersion,
		ID:      &requestID,
		Method:  "tools/list",
		Params:  json.RawMessage(`{"cursor":"a"}`),
	}))

	var response mcpRPCMessage
	require.NoError(t, dec.Decode(&response))
	require.Equal(t, requestID, *response.ID)
	require.JSONEq(t, `{"method":"tools/list","ok":true}`, string(response.Result))
}

func TestMCPBridgeForwardsACPMessagesToProxy(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &rawMCPACPClient{}
	clientConn := connectAgentRawForTest(t, agent, client.handle)

	bridge, err := agent.newMCPSessionBridge(ctx, "session-1", map[string]struct{}{"server-1": {}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })

	_, enc, dec := connectMCPProxyForTest(t, bridge, "server-1")
	require.Eventually(t, func() bool {
		return agent.mcpConnection("conn-1") != nil
	}, time.Second, 10*time.Millisecond)

	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := acp.SendRequest[any](clientConn, ctx, acp.AgentMethodMcpMessage, acp.UnstableMessageMcpRequest{
			ConnectionId: "conn-1",
			Method:       "roots/list",
			Params:       map[string]any{"include": "workspace"},
		})
		if err != nil {
			errCh <- err

			return
		}

		resultCh <- result
	}()

	var request mcpRPCMessage
	require.NoError(t, dec.Decode(&request))
	require.NotNil(t, request.ID)
	require.Equal(t, "roots/list", request.Method)
	require.JSONEq(t, `{"include":"workspace"}`, string(request.Params))

	require.NoError(t, enc.Encode(mcpRPCMessage{
		JSONRPC: mcpJSONRPCVersion,
		ID:      request.ID,
		Result:  json.RawMessage(`{"roots":[]}`),
	}))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case result := <-resultCh:
		require.Equal(t, map[string]any{"roots": []any{}}, result)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	require.NoError(t, clientConn.SendNotification(ctx, acp.AgentMethodMcpMessage, acp.UnstableMessageMcpNotification{
		ConnectionId: "conn-1",
		Method:       "notifications/cancelled",
		Params:       map[string]any{"requestId": "1"},
	}))

	var notification mcpRPCMessage
	require.NoError(t, dec.Decode(&notification))
	require.Nil(t, notification.ID)
	require.Equal(t, "notifications/cancelled", notification.Method)
	require.JSONEq(t, `{"requestId":"1"}`, string(notification.Params))
}

type scriptedListener struct {
	firstErr         error
	firstErrReturned chan struct{}
	calls            atomic.Int32
	unblock          chan struct{}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) == 1 {
		if l.firstErrReturned != nil {
			close(l.firstErrReturned)
		}

		return nil, l.firstErr
	}

	<-l.unblock

	return nil, errors.New("closed")
}

func (l *scriptedListener) Close() error {
	close(l.unblock)

	return nil
}

func (l *scriptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type closeErrorListener struct {
	err error
}

func (l closeErrorListener) Accept() (net.Conn, error) {
	return nil, errors.New("closed")
}

func (l closeErrorListener) Close() error {
	return l.err
}

func (l closeErrorListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type temporaryAcceptError string

func (e temporaryAcceptError) Error() string { return string(e) }
func (e temporaryAcceptError) Temporary() bool {
	return true
}

func TestMCPBridgeAcceptHandlesTemporaryError(t *testing.T) {
	listener := &scriptedListener{firstErr: temporaryAcceptError("accept failed"), unblock: make(chan struct{})}
	bridge := &mcpSessionBridge{
		agent: NewAgent(),
		ln:    listener,
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	done := make(chan struct{})
	go func() {
		bridge.accept(context.Background())
		close(done)
	}()

	require.Eventually(t, func() bool {
		return listener.calls.Load() > 1
	}, time.Second, 10*time.Millisecond)
	close(bridge.done)
	require.NoError(t, listener.Close())

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accept did not stop")
	}
}

func TestMCPBridgeAcceptStopsDuringTemporaryErrorBackoff(t *testing.T) {
	firstErrReturned := make(chan struct{})
	listener := &scriptedListener{
		firstErr:         temporaryAcceptError("accept failed"),
		firstErrReturned: firstErrReturned,
		unblock:          make(chan struct{}),
	}
	bridge := &mcpSessionBridge{
		agent: NewAgent(),
		ln:    listener,
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	done := make(chan struct{})
	go func() {
		bridge.accept(context.Background())
		close(done)
	}()

	select {
	case <-firstErrReturned:
	case <-time.After(time.Second):
		t.Fatal("accept did not receive the temporary error")
	}

	close(bridge.done)
	require.NoError(t, listener.Close())

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accept did not stop")
	}
	require.Equal(t, int32(1), listener.calls.Load())
}

func TestMCPBridgeAcceptStopsOnPermanentError(t *testing.T) {
	listener := &scriptedListener{firstErr: errors.New("accept failed"), unblock: make(chan struct{})}
	bridge := &mcpSessionBridge{
		agent: NewAgent(),
		ln:    listener,
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	done := make(chan struct{})
	go func() {
		bridge.accept(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accept did not stop")
	}
	require.Equal(t, int32(1), listener.calls.Load())
	require.NoError(t, listener.Close())
}

type connListener struct {
	conn    net.Conn
	calls   atomic.Int32
	unblock chan struct{}
}

func (l *connListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) == 1 {
		return l.conn, nil
	}

	<-l.unblock

	return nil, errors.New("closed")
}

func (l *connListener) Close() error {
	close(l.unblock)

	return nil
}

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestMCPBridgeAcceptHandlesConnection(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()

	listener := &connListener{conn: left, unblock: make(chan struct{})}
	bridge := &mcpSessionBridge{
		agent: NewAgent(),
		ln:    listener,
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	done := make(chan struct{})
	go func() {
		bridge.accept(context.Background())
		close(done)
	}()

	require.Eventually(t, func() bool {
		return listener.calls.Load() > 1
	}, time.Second, 10*time.Millisecond)
	close(bridge.done)
	require.NoError(t, listener.Close())

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accept did not stop")
	}
}

func TestWaitMCPAcceptRetryStopsWhenDoneClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)

	require.False(t, waitMCPAcceptRetry(done, mcpAcceptBackoffInitial))
	require.False(t, waitMCPAcceptRetry(done, 0))
}

func TestNextMCPAcceptBackoff(t *testing.T) {
	t.Parallel()

	require.Equal(t, mcpAcceptBackoffInitial, nextMCPAcceptBackoff(0))
	require.Equal(t, 2*mcpAcceptBackoffInitial, nextMCPAcceptBackoff(mcpAcceptBackoffInitial))
	require.Equal(t, mcpAcceptBackoffMax, nextMCPAcceptBackoff(mcpAcceptBackoffMax/2))
	require.Equal(t, mcpAcceptBackoffMax, nextMCPAcceptBackoff(mcpAcceptBackoffMax))
}

func TestMCPBridgeHandleConnRejectsBadConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	run := func(agent *Agent, payload string) {
		bridge := &mcpSessionBridge{
			agent: agent,
			token: "secret",
			allowed: map[string]struct{}{
				"server-1": {},
			},
			conns: make(map[*mcpBridgeConn]struct{}),
		}
		left, right := net.Pipe()
		done := make(chan struct{})
		go func() {
			bridge.handleConn(ctx, left)
			close(done)
		}()

		_, err := right.Write([]byte(payload))
		require.NoError(t, err)
		_ = right.Close()

		select {
		case <-done:
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		}
	}

	run(NewAgent(), "{")
	run(NewAgent(), `{"version":1,"token":"wrong","acpId":"server-1"}`+"\n")
	run(NewAgent(), `{"version":2,"token":"secret","acpId":"server-1"}`+"\n")
	run(NewAgent(), `{"version":1,"token":"secret","acpId":"other-server"}`+"\n")
	run(NewAgent(), `{"version":1,"token":"secret","acpId":"server-1"}`+"\n")

	agent := NewAgent()
	agent.setConnection(&stubAgentClient{connectErr: errors.New("connect failed")})
	run(agent, `{"version":1,"token":"secret","acpId":"server-1"}`+"\n")
}

func TestConstantTimeStringEqual(t *testing.T) {
	t.Parallel()

	require.True(t, constantTimeStringEqual("secret", "secret"))
	require.False(t, constantTimeStringEqual("secret", "other!"))
	require.False(t, constantTimeStringEqual("secret", "short"))
}

func TestMCPBridgeHandleConnClosesIfBridgeClosesDuringConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	agent := NewAgent()
	bridge := &mcpSessionBridge{
		agent: agent,
		token: "secret",
		allowed: map[string]struct{}{
			"server-1": {},
		},
		done:  make(chan struct{}),
		ln:    &scriptedListener{unblock: make(chan struct{})},
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	client := &closeBridgeOnConnectClient{disconnected: make(chan struct{})}
	client.onConnect = func() { _ = bridge.Close() }
	agent.setConnection(client)

	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		bridge.handleConn(ctx, left)
		close(done)
	}()

	_, err := right.Write([]byte(`{"version":1,"token":"secret","acpId":"server-1"}` + "\n"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	select {
	case <-client.disconnected:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	require.Empty(t, bridge.conns)
	_ = right.Close()
}

func TestMCPBridgeRunHandlesInvalidInput(t *testing.T) {
	run := func(payload string) {
		agent := NewAgent()
		left, right := net.Pipe()
		session := &mcpSessionBridge{agent: agent, conns: make(map[*mcpBridgeConn]struct{})}
		conn := &mcpBridgeConn{
			agent:        agent,
			session:      session,
			conn:         left,
			dec:          json.NewDecoder(left),
			enc:          json.NewEncoder(left),
			connectionID: "conn-1",
			pending:      make(map[string]chan mcpRPCMessage),
			closed:       make(chan struct{}),
		}
		go conn.run(context.Background())
		_, err := right.Write([]byte(payload))
		require.NoError(t, err)
		_ = right.Close()

		select {
		case <-conn.closed:
		case <-time.After(time.Second):
			t.Fatal("bridge connection did not close")
		}
	}

	run(`{"jsonrpc":"2.0"}` + "\n")
	run("{")
}

func TestMCPBridgeRejectsProxyMessageWhenForwardLimitReached(t *testing.T) {
	agent := NewAgent()
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	conn := &mcpBridgeConn{
		agent:    agent,
		conn:     left,
		enc:      json.NewEncoder(left),
		pending:  make(map[string]chan mcpRPCMessage),
		forwards: make(chan struct{}, mcpMaxForwards),
		closed:   make(chan struct{}),
	}
	for range mcpMaxForwards {
		conn.forwards <- struct{}{}
	}

	rawID := json.RawMessage("1")
	done := make(chan mcpRPCMessage, 1)
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(right).Decode(&msg)
		done <- msg
	}()

	conn.forwardProxyMessageAsync(context.Background(), mcpRPCMessage{
		ID:     &rawID,
		Method: "tools/list",
	})

	select {
	case msg := <-done:
		require.NotNil(t, msg.Error)
		require.Equal(t, -32000, msg.Error.Code)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MCP error")
	}
}

func TestMCPBridgeIgnoresProxyForwardAfterClose(t *testing.T) {
	agent := NewAgent()
	conn := bufferedMCPConn(t, agent, io.Discard)
	conn.Close()

	rawID := json.RawMessage("1")
	conn.forwardProxyMessageAsync(context.Background(), mcpRPCMessage{
		ID:     &rawID,
		Method: "tools/list",
	})
}

func TestMCPBridgeCloseClosesActiveConnections(t *testing.T) {
	agent := NewAgent()
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret"), 0o600))
	bridge := &mcpSessionBridge{
		agent:     agent,
		tokenFile: tokenFile,
		done:      make(chan struct{}),
		conns:     make(map[*mcpBridgeConn]struct{}),
		ln:        &scriptedListener{unblock: make(chan struct{})},
	}
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })

	pending := make(chan mcpRPCMessage)
	conn := &mcpBridgeConn{
		agent:   agent,
		session: bridge,
		conn:    left,
		pending: map[string]chan mcpRPCMessage{"1": pending},
		closed:  make(chan struct{}),
	}
	bridge.conns[conn] = struct{}{}

	require.NoError(t, bridge.Close())
	require.NoError(t, bridge.Close())

	select {
	case <-conn.closed:
	default:
		t.Fatal("connection was not closed")
	}
	select {
	case _, ok := <-pending:
		require.False(t, ok)
	default:
		t.Fatal("pending channel was not closed")
	}
	_, err := os.Stat(tokenFile)
	require.ErrorIs(t, err, os.ErrNotExist)

	agentWithDisconnectErr := NewAgent()
	agentWithDisconnectErr.setConnection(&stubAgentClient{disconnectErr: errors.New("disconnect failed")})
	left2, right2 := net.Pipe()
	t.Cleanup(func() { _ = right2.Close() })
	conn2 := &mcpBridgeConn{
		agent:        agentWithDisconnectErr,
		session:      &mcpSessionBridge{agent: agentWithDisconnectErr, conns: make(map[*mcpBridgeConn]struct{})},
		conn:         left2,
		connectionID: "conn-1",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	conn2.close(context.Background())
}

func TestMCPBridgeCloseReturnsCleanupErrors(t *testing.T) {
	listenerErr := errors.New("listener close failed")
	tokenDir := filepath.Join(t.TempDir(), "token-dir")
	require.NoError(t, os.Mkdir(tokenDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tokenDir, "child"), []byte("x"), 0o600))

	bridge := &mcpSessionBridge{
		agent:     NewAgent(),
		tokenFile: tokenDir,
		done:      make(chan struct{}),
		conns:     make(map[*mcpBridgeConn]struct{}),
		ln:        closeErrorListener{err: listenerErr},
	}

	err := bridge.Close()
	require.ErrorIs(t, err, listenerErr)
	require.Contains(t, err.Error(), "token-dir")
	require.NoError(t, bridge.Close())

	bridge = &mcpSessionBridge{
		agent: NewAgent(),
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}
	require.NoError(t, bridge.Close())
}

func TestMCPBridgeRecoverAcceptClosesBridge(t *testing.T) {
	t.Parallel()

	bridge := &mcpSessionBridge{
		agent: NewAgent(WithLogger(slog.New(slog.DiscardHandler))),
		ln:    closeErrorListener{err: errors.New("listener close failed")},
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}

	func() {
		defer bridge.recoverAccept(context.Background())

		panic("boom")
	}()

	select {
	case <-bridge.done:
	default:
		t.Fatal("bridge was not closed")
	}
}

func TestMCPBridgeRecoverConnectionClosesBridge(t *testing.T) {
	t.Parallel()

	bridge := &mcpSessionBridge{
		agent: NewAgent(WithLogger(slog.New(slog.DiscardHandler))),
		ln:    closeErrorListener{err: errors.New("listener close failed")},
		done:  make(chan struct{}),
		conns: make(map[*mcpBridgeConn]struct{}),
	}

	func() {
		defer bridge.recoverConnection(context.Background())

		panic("boom")
	}()

	select {
	case <-bridge.done:
	default:
		t.Fatal("bridge was not closed")
	}
}

func TestMCPBridgeCloseWaitsBeforeRemovingTokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret"), 0o600))

	bridge := &mcpSessionBridge{
		agent:     NewAgent(),
		tokenFile: tokenFile,
		done:      make(chan struct{}),
		conns:     make(map[*mcpBridgeConn]struct{}),
	}
	bridge.wg.Add(1)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- bridge.Close()
	}()

	select {
	case err := <-closeDone:
		require.NoError(t, err)
		t.Fatal("bridge close returned before in-flight bridge work finished")
	case <-time.After(20 * time.Millisecond):
	}

	_, err := os.Stat(tokenFile)
	require.NoError(t, err)

	bridge.wg.Done()
	require.NoError(t, <-closeDone)

	_, err = os.Stat(tokenFile)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMCPBridgeCloseCancelsAndWaitsForForwards(t *testing.T) {
	started := make(chan struct{})
	agent := NewAgent()
	agent.setConnection(&stubAgentClient{
		messageHook: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
		},
	})
	conn := bufferedMCPConn(t, agent, io.Discard)
	forwardCtx, cancelForwardCtx := conn.connectionContext(context.Background())
	defer cancelForwardCtx()

	id := json.RawMessage(`"1"`)
	conn.forwardProxyMessageAsync(forwardCtx, mcpRPCMessage{ID: &id, Method: "tools/list"})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MCP forward")
	}

	closed := make(chan struct{})
	go func() {
		conn.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("MCP connection close did not wait for and cancel forward")
	}
}

func TestMCPBridgeRecoverForwardClosesConnection(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))
	conn := bufferedMCPConn(t, agent, io.Discard)

	func() {
		defer conn.recoverForward(context.Background(), "test forward")

		panic("boom")
	}()

	select {
	case <-conn.closed:
	default:
		t.Fatal("connection was not closed")
	}
}

func TestMCPBridgeRecoverProxyReader(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))
	conn := bufferedMCPConn(t, agent, io.Discard)

	func() {
		defer conn.recoverProxyReader(context.Background())

		panic("boom")
	}()

	select {
	case <-conn.closed:
	default:
		t.Fatal("connection was not closed")
	}
}

func bufferedMCPConn(t *testing.T, agent *Agent, writer io.Writer) *mcpBridgeConn {
	t.Helper()

	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	session := &mcpSessionBridge{agent: agent, conns: make(map[*mcpBridgeConn]struct{})}

	return &mcpBridgeConn{
		agent:        agent,
		session:      session,
		conn:         left,
		dec:          json.NewDecoder(left),
		enc:          json.NewEncoder(writer),
		connectionID: "conn-1",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
}

func TestMCPBridgeProxyForwardingErrorBranches(t *testing.T) {
	ctx := context.Background()
	id := json.RawMessage(`"1"`)

	var out bytes.Buffer
	agent := NewAgent()
	conn := bufferedMCPConn(t, agent, &out)
	conn.forwardProxyMessage(ctx, mcpRPCMessage{ID: &id, Method: "tools/list", Params: json.RawMessage(`[]`)})
	var response mcpRPCMessage
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response))
	require.Equal(t, -32602, response.Error.Code)

	out.Reset()
	conn = bufferedMCPConn(t, agent, &out)
	conn.forwardProxyMessage(ctx, mcpRPCMessage{ID: &id, Method: "tools/list"})
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response))
	require.Equal(t, -32603, response.Error.Code)

	agent.setConnection(&stubAgentClient{notifyErr: errors.New("notify failed")})
	conn.forwardProxyMessage(ctx, mcpRPCMessage{Method: "notifications/progress"})

	out.Reset()
	agent.setConnection(&stubAgentClient{messageErr: acp.NewInvalidParams(map[string]any{"bad": true})})
	conn = bufferedMCPConn(t, agent, &out)
	conn.forwardProxyMessage(ctx, mcpRPCMessage{ID: &id, Method: "tools/list"})
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response))
	require.Equal(t, -32602, response.Error.Code)

	out.Reset()
	require.Error(t, conn.sendMCPResult(id, func() {}))
	require.NoError(t, conn.sendMCPResult(id, nil))
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response))
	require.Equal(t, id, *response.ID)

	out.Reset()
	require.Error(t, conn.sendMCPError(id, -1, "bad", func() {}))
	require.NoError(t, conn.sendMCPError(id, -1, "bad", map[string]any{"x": "y"}))
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response))
	require.Equal(t, json.RawMessage(`{"x":"y"}`), response.Error.Data)

	var logs bytes.Buffer
	logAgent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	conn = bufferedMCPConn(t, logAgent, failingWriter{})
	conn.forwardProxyMessage(ctx, mcpRPCMessage{ID: &id, Method: "tools/list", Params: json.RawMessage(`[]`)})
	require.Contains(t, logs.String(), "send MCP proxy response failed")
}

func TestMCPBridgeForwardACPContextCancellationEdges(t *testing.T) {
	agent := NewAgent()
	conn := bufferedMCPConn(t, agent, io.Discard)

	notificationCtx, cancelNotification := context.WithCancel(context.Background())
	err := conn.forwardACPNotification(notificationCtx, acp.UnstableMessageMcpRequest{
		ConnectionId: "conn-1",
		Method:       "notifications/progress",
		Params:       map[string]any{"cancel": cancelOnMarshal{cancel: cancelNotification}},
	})
	require.ErrorIs(t, err, context.Canceled)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	_, err = conn.forwardACPRequest(requestCtx, acp.UnstableMessageMcpRequest{
		ConnectionId: "conn-1",
		Method:       "tools/list",
		Params:       map[string]any{"cancel": cancelOnMarshal{cancel: cancelRequest}},
	})
	require.ErrorIs(t, err, context.Canceled)

	conn.mu.Lock()
	defer conn.mu.Unlock()
	require.Empty(t, conn.pending)
}

func TestMCPBridgeACPMessagingErrorBranches(t *testing.T) {
	agent := NewAgent()
	id := json.RawMessage(`"1"`)

	_, reqErr := agent.handleMCPMessage(context.Background(), json.RawMessage(`{`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = agent.handleMCPMessage(context.Background(), json.RawMessage(`{"connectionId":"conn-1"}`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = agent.handleMCPMessage(
		context.Background(),
		json.RawMessage(`{"connectionId":"conn-1","method":"tools/list"}`),
	)
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)

	conn := bufferedMCPConn(t, agent, failingWriter{})
	agent.registerMCPConnection(conn)
	_, reqErr = agent.handleMCPMessage(
		context.Background(),
		json.RawMessage(`{"connectionId":"conn-1","method":"notifications/progress"}`),
	)
	require.NotNil(t, reqErr)

	_, reqErr = agent.handleMCPMessage(
		context.Background(),
		json.RawMessage(`{"connectionId":"conn-1","method":"tools/list"}`),
	)
	require.NotNil(t, reqErr)

	conn = bufferedMCPConn(t, agent, io.Discard)
	err := conn.forwardACPNotification(context.Background(), acp.UnstableMessageMcpRequest{
		Method: "notifications/progress",
		Params: map[string]any{
			"bad": func() {},
		},
	})
	require.Error(t, err)

	_, err = conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{
		Method: "tools/list",
		Params: map[string]any{
			"bad": func() {},
		},
	})
	require.Error(t, err)

	conn = bufferedMCPConn(t, agent, failingWriter{})
	_, err = conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
	require.Error(t, err)

	conn = bufferedMCPConn(t, agent, io.Discard)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = conn.forwardACPRequest(cancelCtx, acp.UnstableMessageMcpRequest{Method: "tools/list"})
	require.ErrorIs(t, err, context.Canceled)

	conn = bufferedMCPConn(t, agent, io.Discard)
	close(conn.closed)
	_, err = conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
	require.Error(t, err)
	require.Empty(t, conn.pending)

	conn = bufferedMCPConn(t, agent, io.Discard)
	for i := range mcpMaxPending {
		conn.pending[strconv.Itoa(i)] = make(chan mcpRPCMessage, 1)
	}
	_, err = conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
	require.ErrorContains(t, err, "too many pending")

	conn = bufferedMCPConn(t, agent, io.Discard)
	errCh := make(chan error, 1)
	go func() {
		_, forwardErr := conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
		errCh <- forwardErr
	}()
	require.Eventually(t, func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()

		return len(conn.pending) == 1
	}, time.Second, 10*time.Millisecond)
	conn.mu.Lock()
	for _, ch := range conn.pending {
		ch <- mcpRPCMessage{ID: &id, Error: &mcpRPCError{Code: -32000, Message: "no"}}
	}
	conn.mu.Unlock()
	require.Error(t, <-errCh)

	conn = bufferedMCPConn(t, agent, io.Discard)
	errCh = make(chan error, 1)
	go func() {
		_, forwardErr := conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
		errCh <- forwardErr
	}()
	require.Eventually(t, func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()

		return len(conn.pending) == 1
	}, time.Second, 10*time.Millisecond)
	conn.mu.Lock()
	for _, ch := range conn.pending {
		close(ch)
	}
	conn.mu.Unlock()
	require.Error(t, <-errCh)
}

func TestMCPBridgeForwardACPRequestTimesOutPendingResponse(t *testing.T) {
	conn := bufferedMCPConn(t, NewAgent(), io.Discard)
	conn.timeout = 10 * time.Millisecond

	_, err := conn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, conn.pending)
}

func TestMCPBridgeACPMessagingUsesSessionCancel(t *testing.T) {
	t.Parallel()

	t.Run("request", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		sessionID := acp.SessionId("session-1")
		turnCtx, cancelTurn := context.WithCancel(context.Background())
		session := &Session{id: sessionID, turnDone: turnCtx.Done()}

		agent.mu.Lock()
		agent.sessions[sessionID] = session
		agent.mu.Unlock()

		conn := bufferedMCPConn(t, agent, io.Discard)
		conn.session.session = sessionID
		agent.registerMCPConnection(conn)

		errCh := make(chan *acp.RequestError, 1)
		go func() {
			_, reqErr := agent.handleMCPMessage(
				context.Background(),
				json.RawMessage(`{"connectionId":"conn-1","method":"tools/list"}`),
			)
			errCh <- reqErr
		}()

		require.Eventually(t, func() bool {
			conn.mu.Lock()
			defer conn.mu.Unlock()

			return len(conn.pending) == 1
		}, time.Second, 10*time.Millisecond)

		cancelTurn()

		select {
		case reqErr := <-errCh:
			require.NotNil(t, reqErr)
			require.Equal(t, -32800, reqErr.Code)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for MCP forward cancellation")
		}
	})

	t.Run("notification", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		sessionID := acp.SessionId("session-1")
		turnCtx, cancelTurn := context.WithCancel(context.Background())
		session := &Session{id: sessionID, turnDone: turnCtx.Done()}

		agent.mu.Lock()
		agent.sessions[sessionID] = session
		agent.mu.Unlock()

		var out bytes.Buffer
		conn := bufferedMCPConn(t, agent, &out)
		conn.session.session = sessionID
		agent.registerMCPConnection(conn)
		cancelTurn()

		_, reqErr := agent.handleMCPMessage(
			context.Background(),
			json.RawMessage(`{"connectionId":"conn-1","method":"notifications/progress"}`),
		)
		require.NotNil(t, reqErr)
		require.Equal(t, -32800, reqErr.Code)
		require.Empty(t, out.String())
	})
}

func TestMCPBridgeHelpers(t *testing.T) {
	require.Empty(t, mcpIDKey(nil))

	raw, err := marshalMCPParams(nil)
	require.NoError(t, err)
	require.Equal(t, json.RawMessage("null"), raw)

	_, err = marshalMCPParams(map[string]any{"bad": func() {}})
	require.Error(t, err)

	params, err := mcpParamsMap(nil)
	require.NoError(t, err)
	require.Empty(t, params)

	params, err = mcpParamsMap(json.RawMessage("null"))
	require.NoError(t, err)
	require.Empty(t, params)

	_, err = mcpParamsMap(json.RawMessage("[]"))
	require.Error(t, err)
	require.Empty(t, mcpProtocolVersionFromRawParams(json.RawMessage("[]")))
	require.Empty(t, mcpProtocolVersionFromParams(nil))
	require.Equal(
		t,
		"2025-06-18",
		mcpProtocolVersionFromRawParams(json.RawMessage(`{"protocolVersion":"2025-06-18"}`)),
	)

	result, err := unmarshalMCPResult(nil)
	require.NoError(t, err)
	require.Equal(t, json.RawMessage("null"), result)

	_, err = unmarshalMCPResult(json.RawMessage("{"))
	require.Error(t, err)
}

func TestRunMCPProxyPipesLines(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr

			return
		}
		defer conn.Close()

		dec := json.NewDecoder(conn)

		var hello mcpProxyHello
		if decodeErr := dec.Decode(&hello); decodeErr != nil {
			serverDone <- decodeErr

			return
		}
		if hello.Version != mcpProxyVersion || hello.Token != "secret" || hello.ACPID != "server-1" {
			serverDone <- io.ErrUnexpectedEOF

			return
		}

		var line json.RawMessage
		readErr := dec.Decode(&line)
		if readErr != nil {
			serverDone <- readErr

			return
		}
		if !bytes.Equal(line, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}")) {
			serverDone <- io.ErrUnexpectedEOF

			return
		}
		_, writeErr := conn.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"pong\"}\n"))
		serverDone <- writeErr
	}()

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	t.Cleanup(func() {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	})

	outputCh := make(chan string, 1)
	go func() {
		line, readErr := bufio.NewReader(stdoutReader).ReadString('\n')
		if readErr == nil {
			outputCh <- line
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunMCPProxy(ctx, stdinReader, stdoutWriter, MCPProxyOptions{
			Network: "tcp",
			Address: ln.Addr().String(),
			Token:   "secret",
			ACPID:   "server-1",
		})
	}()

	_, err = stdinWriter.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\n"))
	require.NoError(t, err)

	select {
	case output := <-outputCh:
		require.Equal(t, "{\"jsonrpc\":\"2.0\",\"method\":\"pong\"}\n", output)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	require.NoError(t, stdinWriter.Close())

	select {
	case err := <-serverDone:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
}

func TestRunMCPProxyErrors(t *testing.T) {
	originalDial := mcpDialContext
	t.Cleanup(func() { mcpDialContext = originalDial })

	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial failed")
	}
	err := RunMCPProxy(context.Background(), strings.NewReader(""), io.Discard, MCPProxyOptions{Network: "tcp", Address: "x"})
	require.Error(t, err)

	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		return writeFailConn{}, nil
	}
	err = RunMCPProxy(context.Background(), strings.NewReader(""), io.Discard, MCPProxyOptions{Network: "tcp", Address: "x"})
	require.Error(t, err)

	leftErr, rightErr := net.Pipe()
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		return leftErr, nil
	}
	helloErrCh := make(chan error, 1)
	go func() {
		var hello mcpProxyHello
		helloErrCh <- json.NewDecoder(rightErr).Decode(&hello)
	}()
	err = RunMCPProxy(context.Background(), errReader{err: errors.New("read failed")}, io.Discard, MCPProxyOptions{
		Network: "tcp",
		Address: "x",
		Token:   "secret",
		ACPID:   "server-1",
	})
	require.Error(t, err)
	require.NoError(t, <-helloErrCh)
	_ = rightErr.Close()

	left, right := net.Pipe()
	mcpDialContext = func(context.Context, string, string) (net.Conn, error) {
		return left, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunMCPProxy(ctx, stdinReader, stdoutWriter, MCPProxyOptions{
			Network: "tcp",
			Address: "x",
			Token:   "secret",
			ACPID:   "server-1",
		})
	}()

	var hello mcpProxyHello
	require.NoError(t, json.NewDecoder(right).Decode(&hello))
	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)
	_ = stdinWriter.Close()
	_ = stdinReader.Close()
	_ = stdoutWriter.Close()
	_ = stdoutReader.Close()
	_ = right.Close()
}

type writeFailConn struct{}

func (writeFailConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (writeFailConn) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (writeFailConn) Close() error                     { return nil }
func (writeFailConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (writeFailConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (writeFailConn) SetDeadline(time.Time) error      { return nil }
func (writeFailConn) SetReadDeadline(time.Time) error  { return nil }
func (writeFailConn) SetWriteDeadline(time.Time) error { return nil }

func TestProxyCopyErrors(t *testing.T) {
	errCh := make(chan error, 1)
	proxyCopy(errCh, failingWriter{}, strings.NewReader("line\n"))
	require.Error(t, <-errCh)

	proxyCopy(errCh, io.Discard, errReader{err: errors.New("read failed")})
	require.Error(t, <-errCh)
}
