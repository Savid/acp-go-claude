package claudeacp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestLocalAgentConnectionClientMethods(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 2}))
	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setConnection(conn)

	_, err := clientConn.Initialize(ctx, acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotNil(t, conn.Done())

	require.NoError(t, conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "e1"}))
	_, err = conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"}})
	require.NoError(t, err)
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{}, elicitationScope{})
	require.ErrorContains(t, err, "form or url")
	requestIDStr := acp.RequestIdStr("r1")
	requestID := acp.RequestId{Str: &requestIDStr}
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{ElicitationId: "e2", Message: "m", Mode: "url", Url: "https://example.test", Meta: map[string]any{"u": "m"}}}, elicitationScope{SessionID: "s", ToolCallID: "tool-1", RequestID: &requestID})
	require.NoError(t, err)
	_, err = conn.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "/tmp/file"})
	require.Error(t, err)
	_, err = conn.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "/tmp/file", Content: "body"})
	require.Error(t, err)
	permission, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
	})
	require.NoError(t, err)
	require.NotNil(t, permission.Outcome.Cancelled)
	require.NoError(t, conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: "s", Update: acp.UpdateAgentMessageText("hello")}))
	_, err = conn.CreateTerminal(ctx, acp.CreateTerminalRequest{})
	require.Error(t, err)
	_, err = conn.KillTerminal(ctx, acp.KillTerminalRequest{})
	require.Error(t, err)
	_, err = conn.TerminalOutput(ctx, acp.TerminalOutputRequest{})
	require.Error(t, err)
	_, err = conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{})
	require.Error(t, err)
	_, err = conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{})
	require.Error(t, err)
	require.NoError(t, conn.NotifyExtension(ctx, "_client/test", map[string]any{"ok": true}))
	require.Error(t, conn.NotifyExtension(ctx, "bad", nil))
}

func TestLocalAgentDispatcherBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	ctx := context.Background()

	_, reqErr := conn.handle(ctx, acp.AgentMethodSessionList, nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32600, reqErr.Code)

	resp, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{}`))
	require.Nil(t, reqErr)
	require.NotNil(t, resp)
	require.True(t, conn.initialized.Load())

	_, reqErr = conn.handle(ctx, "unknown", nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)
	_, reqErr = conn.handle(ctx, "_unknown", nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)
	_, reqErr = conn.handle(ctx, acp.AgentMethodAuthenticate, json.RawMessage(`{bad`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)
	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionNew, json.RawMessage(`{"cwd":"relative"}`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = localResponse((*Agent).Authenticate)(ctx, agent, json.RawMessage(`{"methodId":"m"}`))
	require.NotNil(t, reqErr)
	_, reqErr = localNotification((*Agent).Cancel)(ctx, agent, json.RawMessage(`{"sessionId":"missing"}`))
	require.NotNil(t, reqErr)
	_, reqErr = localNotification((*Agent).Cancel)(ctx, agent, json.RawMessage(`{bad`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)
	require.Equal(t, acp.NewInvalidParams(map[string]any{"x": "y"}), lifecycleMetaError(acp.NewInvalidParams(map[string]any{"x": "y"})))
	require.Error(t, lifecycleMetaError(context.Canceled))
}

func TestLocalAgentConnectionClientBackpressure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	release, err := agent.acquireClientCall(ctx)
	require.NoError(t, err)
	defer release()

	conn := &localAgentConnection{agent: agent}

	requireBackpressure := func(t *testing.T, err error) {
		t.Helper()
		var reqErr *acp.RequestError
		require.ErrorAs(t, err, &reqErr)
		data, ok := reqErr.Data.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "client_call", data["limit"])
	}

	requireBackpressure(t, conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "e1"}))
	_, err = conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"}})
	requireBackpressure(t, err)
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{ElicitationId: "e2", Message: "m", Mode: "url", Url: "https://example.test"}}, elicitationScope{})
	requireBackpressure(t, err)
	_, err = conn.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "/tmp/file"})
	requireBackpressure(t, err)
	_, err = conn.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "/tmp/file", Content: "body"})
	requireBackpressure(t, err)
	_, err = conn.RequestPermission(ctx, acp.RequestPermissionRequest{})
	requireBackpressure(t, err)
	requireBackpressure(t, conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: "s", Update: acp.UpdateAgentMessageText("x")}))
	_, err = conn.CreateTerminal(ctx, acp.CreateTerminalRequest{})
	requireBackpressure(t, err)
	_, err = conn.KillTerminal(ctx, acp.KillTerminalRequest{})
	requireBackpressure(t, err)
	_, err = conn.TerminalOutput(ctx, acp.TerminalOutputRequest{})
	requireBackpressure(t, err)
	_, err = conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{})
	requireBackpressure(t, err)
	_, err = conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{})
	requireBackpressure(t, err)
	requireBackpressure(t, conn.NotifyExtension(ctx, "_client/test", map[string]any{"ok": true}))
}

func TestConnectionInputGate(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	gate := newConnectionInputGate(reader)
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4)
		n, _ := gate.Read(buf)
		done <- buf[:n]
	}()

	select {
	case <-done:
		t.Fatal("gate read before open")
	default:
	}

	gate.open()
	gate.open()
	_, err := writer.Write([]byte("test"))
	require.NoError(t, err)
	require.Equal(t, []byte("test"), <-done)
	require.NoError(t, writer.Close())
	require.NoError(t, reader.Close())
}
