package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
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

	forkAgent := newForkTestAgent(t, nil)
	forkConn := &localAgentConnection{agent: forkAgent}
	forkConn.initialized.Store(true)
	rawFork, err := json.Marshal(ForkSessionRequest("parent", t.TempDir()))
	require.NoError(t, err)
	resp, reqErr = forkConn.handle(ctx, ForkSessionMethod, rawFork)
	require.Nil(t, reqErr)
	require.NotNil(t, resp)

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

func TestLifecycleCommandUpdatePostResponseHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		params json.RawMessage
		result any
	}{
		{
			name:   "new",
			method: acp.AgentMethodSessionNew,
			result: acp.NewSessionResponse{SessionId: "session-1"},
		},
		{
			name:   "load",
			method: acp.AgentMethodSessionLoad,
			params: json.RawMessage(`{"sessionId":"session-1"}`),
			result: acp.LoadSessionResponse{},
		},
		{
			name:   "resume",
			method: acp.AgentMethodSessionResume,
			params: json.RawMessage(`{"sessionId":"session-1"}`),
			result: acp.ResumeSessionResponse{},
		},
		{
			name:   "fork",
			method: ForkSessionMethod,
			result: acp.UnstableForkSessionResponse{SessionId: "session-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			agent := NewAgent()
			client := newRecordingAgentClient()
			agent.setConnection(client)
			session := &agentSession{
				agent:             agent,
				id:                "session-1",
				availableCommands: []claude.SlashCommand{{Name: "help", Description: "Help"}},
			}
			agent.mu.Lock()
			agent.sessions[session.id] = session
			agent.mu.Unlock()

			hooks := &postResponseHooks{log: agent.log}
			conn := &localAgentConnection{agent: agent, hooks: hooks}
			conn.enqueueLifecycleCommandHook(ctx, tc.method, tc.params, tc.result)

			resultJSON, err := json.Marshal(tc.result)
			require.NoError(t, err)
			var output bytes.Buffer
			wrapped := hooks.wrap(&output)
			_, err = wrapped.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + "}\n"))
			require.NoError(t, err)
			require.Contains(t, output.String(), `"id":1`)

			require.Eventually(t, func() bool {
				return len(availableCommandUpdates(client.Updates())) == 1
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestLifecycleCommandPostResponseHookBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setConnection(client)

	connWithoutHooks := &localAgentConnection{agent: agent}
	connWithoutHooks.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, nil, acp.NewSessionResponse{SessionId: "session-1"})

	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionList, nil, acp.ListSessionsResponse{})
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionLoad, json.RawMessage(`{bad`), acp.LoadSessionResponse{})

	badMarshal := acp.NewSessionResponse{
		SessionId: "session-1",
		Meta:      map[string]any{"bad": make(chan struct{})},
	}
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, nil, badMarshal)

	result := acp.NewSessionResponse{SessionId: "missing"}
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, nil, result)
	resultJSON, err := json.Marshal(result)
	require.NoError(t, err)
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + `}`))

	require.Never(t, func() bool {
		return len(availableCommandUpdates(client.Updates())) > 0
	}, 50*time.Millisecond, 5*time.Millisecond)

	failAgent := NewAgent()
	failClient := newRecordingAgentClient()
	failClient.sessionUpdateErr = errors.New("post update failed")
	failAgent.setConnection(failClient)
	failSession := &agentSession{
		agent:             failAgent,
		id:                "session-1",
		availableCommands: []claude.SlashCommand{{Name: "help"}},
	}
	failAgent.sessions[failSession.id] = failSession
	failHooks := &postResponseHooks{log: failAgent.log}
	failConn := &localAgentConnection{agent: failAgent, hooks: failHooks}
	failResult := acp.NewSessionResponse{SessionId: failSession.id}
	failConn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, nil, failResult)
	failResultJSON, err := json.Marshal(failResult)
	require.NoError(t, err)
	failHooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(failResultJSON) + `}`))

	require.Never(t, func() bool {
		return len(availableCommandUpdates(failClient.Updates())) > 0
	}, 50*time.Millisecond, 5*time.Millisecond)

	hooks.runAfterResponseWrite([]byte(`{bad`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","method":"session/update"}`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"}}`))
	hooks.enqueue(json.RawMessage(`{"ok":true}`), func() {})
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{"other":true}}`))

	_, ok := lifecycleCommandSessionID("unknown", nil, nil)
	require.False(t, ok)
	_, ok = lifecycleCommandSessionID(acp.AgentMethodSessionResume, json.RawMessage(`{bad`), acp.ResumeSessionResponse{})
	require.False(t, ok)
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
