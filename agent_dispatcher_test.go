package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	recording := &recordingClient{}
	clientConn := acp.NewClientSideConnection(recording, c2aW, a2cR)
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setConnection(conn)

	_, err := clientConn.Initialize(ctx, acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotNil(t, conn.Done())

	require.NoError(t, conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "e1"}))
	_, err = conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"}})
	require.ErrorContains(t, err, "sessionId and turnNonce")
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{}, elicitationScope{})
	require.ErrorContains(t, err, "form or url")
	requestID := "r1"
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{ElicitationId: "e2", Message: "m", Mode: "url", Url: "https://example.test", Meta: map[string]any{"u": "m"}}}, elicitationScope{SessionID: "s", TurnNonce: "turn-1", RequestID: &requestID})
	require.NoError(t, err)
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "approve", Mode: "form", Meta: map[string]any{"f": "m"}}}, elicitationScope{SessionID: "s", TurnNonce: "turn-2", ToolCallID: "tool-1"})
	require.NoError(t, err)
	elicitations := recording.Elicitations()
	require.Len(t, elicitations, 2)
	require.Equal(t, map[string]any{
		"u": "m",
		routeMetaKey: map[string]any{
			routeFieldVer:  float64(1),
			routeFieldID:   "s",
			routeFieldTurn: "turn-1",
			"requestId":    "r1",
		},
	}, elicitations[0].Url.Meta)
	require.Equal(t, map[string]any{
		"f": "m",
		routeMetaKey: map[string]any{
			routeFieldVer:  float64(1),
			routeFieldID:   "s",
			routeFieldTurn: "turn-2",
			"toolCallId":   "tool-1",
		},
	}, elicitations[1].Form.Meta)
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
}

func TestLocalAgentDispatcherRejectsClosedBeforeDispatchOrDecode(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	require.NoError(t, agent.Close())
	ctx := context.Background()

	tests := []struct {
		name        string
		initialized bool
		method      string
		params      json.RawMessage
	}{
		{name: "before initialization gate", method: acp.AgentMethodSessionList},
		{name: "initialize", method: acp.AgentMethodInitialize, params: json.RawMessage(`{bad`)},
		{name: "unknown stable method", initialized: true, method: "unknown"},
		{name: "unknown extension method", initialized: true, method: "_unknown"},
		{name: "known malformed params", initialized: true, method: acp.AgentMethodAuthenticate, params: json.RawMessage(`{bad`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn := &localAgentConnection{agent: agent}
			conn.initialized.Store(tc.initialized)

			_, reqErr := conn.handle(ctx, tc.method, tc.params)
			require.NotNil(t, reqErr)
			require.Equal(t, -32600, reqErr.Code)
			require.Equal(t, map[string]any{jsonFieldError: errAgentClosed.Error()}, reqErr.Data)
		})
	}
}

func TestStableSessionForkReturnsMethodNotFound(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	ctx := context.Background()

	raw, err := json.Marshal(ForkSessionRequest("parent", t.TempDir()))
	require.NoError(t, err)

	// The adapter exposes fork only through the namespaced extension method; the
	// stable ACP session/fork route must be method-not-found (-32601).
	_, reqErr := conn.handle(ctx, acp.AgentMethodSessionFork, raw)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)

	_, extErr := agent.HandleExtensionMethod(ctx, acp.AgentMethodSessionFork, raw)
	require.Error(t, extErr)

	var extReqErr *acp.RequestError
	require.ErrorAs(t, extErr, &extReqErr)
	require.Equal(t, -32601, extReqErr.Code)
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
		require.Equal(t, "client_calls", data["limit"])
	}

	requireBackpressure(t, conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "e1"}))
	_, err = conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "m", Mode: "form"}})
	require.ErrorContains(t, err, "sessionId and turnNonce")
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{ElicitationId: "e2", Message: "m", Mode: "url", Url: "https://example.test"}}, elicitationScope{SessionID: "s", TurnNonce: "turn-1"})
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
			params: postResponseHookParams(nil, "1"),
			result: acp.NewSessionResponse{SessionId: "session-1"},
		},
		{
			name:   "load",
			method: acp.AgentMethodSessionLoad,
			params: postResponseHookParams(map[string]string{"sessionId": "session-1"}, "1"),
			result: acp.LoadSessionResponse{},
		},
		{
			name:   "resume",
			method: acp.AgentMethodSessionResume,
			params: postResponseHookParams(map[string]string{"sessionId": "session-1"}, "1"),
			result: acp.ResumeSessionResponse{},
		},
		{
			name:   "fork",
			method: ForkSessionMethod,
			params: postResponseHookParams(nil, "1"),
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

func TestLifecycleCommandPostResponseHookUsesResponseIDForIdenticalResults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setConnection(client)
	sessionOne := &agentSession{
		agent:             agent,
		id:                "session-1",
		availableCommands: []claude.SlashCommand{{Name: "one", Description: "One"}},
	}
	sessionTwo := &agentSession{
		agent:             agent,
		id:                "session-2",
		availableCommands: []claude.SlashCommand{{Name: "two", Description: "Two"}},
	}
	agent.mu.Lock()
	agent.sessions[sessionOne.id] = sessionOne
	agent.sessions[sessionTwo.id] = sessionTwo
	agent.mu.Unlock()

	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	conn.enqueueLifecycleCommandHook(
		ctx,
		acp.AgentMethodSessionResume,
		postResponseHookParams(map[string]string{"sessionId": string(sessionOne.id)}, "1"),
		acp.ResumeSessionResponse{},
	)
	conn.enqueueLifecycleCommandHook(
		ctx,
		acp.AgentMethodSessionResume,
		postResponseHookParams(map[string]string{"sessionId": string(sessionTwo.id)}, "2"),
		acp.ResumeSessionResponse{},
	)

	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	require.Eventually(t, func() bool {
		updates := availableCommandUpdates(client.Updates())

		return len(updates) == 1 && len(updates[0].AvailableCommands) == 1 && updates[0].AvailableCommands[0].Name == "two"
	}, time.Second, 10*time.Millisecond)

	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	require.Eventually(t, func() bool {
		updates := availableCommandUpdates(client.Updates())

		return len(updates) == 2 && len(updates[1].AvailableCommands) == 1 && updates[1].AvailableCommands[0].Name == "one"
	}, time.Second, 10*time.Millisecond)
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
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, nil, acp.NewSessionResponse{SessionId: "session-1"})
	require.Empty(t, postResponseHookRequestID(json.RawMessage(`{bad`)))

	result := acp.NewSessionResponse{SessionId: "missing"}
	conn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), result)
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
	failConn.enqueueLifecycleCommandHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), failResult)
	failResultJSON, err := json.Marshal(failResult)
	require.NoError(t, err)
	failHooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(failResultJSON) + `}`))

	require.Never(t, func() bool {
		return len(availableCommandUpdates(failClient.Updates())) > 0
	}, 50*time.Millisecond, 5*time.Millisecond)

	hooks.runAfterResponseWrite([]byte(`{bad`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","method":"session/update"}`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"}}`))
	hooks.enqueue("1", func() {})
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{"other":true}}`))

	_, ok := lifecycleCommandSessionID("unknown", nil, nil)
	require.False(t, ok)
	_, ok = lifecycleCommandSessionID(acp.AgentMethodSessionResume, json.RawMessage(`{bad`), acp.ResumeSessionResponse{})
	require.False(t, ok)
}

func TestPostResponseHookRecoversPanic(t *testing.T) {
	t.Parallel()

	ran := false
	runPostResponseHook(slog.New(slog.DiscardHandler), func() {
		defer func() { ran = true }()

		panic("hook panic")
	})
	require.True(t, ran)
}

func TestPostResponseHookRequestReaderTagsLifecycleRequests(t *testing.T) {
	t.Parallel()

	tagged, err := io.ReadAll(newPostResponseHookRequestReader(bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{"sessionId":"session-1"}}` + "\n",
	)))
	require.NoError(t, err)

	var msg struct {
		Params map[string]string `json:"params"`
	}
	require.NoError(t, json.Unmarshal(tagged, &msg))
	require.Equal(t, "7", msg.Params[postResponseHookIDParam])

	smallReader := newPostResponseHookRequestReader(bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":8,"method":"session/resume","params":{"sessionId":"session-1"}}`,
	))
	buf := make([]byte, 5)
	n, err := smallReader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	rest, err := io.ReadAll(smallReader)
	require.NoError(t, err)
	require.Contains(t, string(buf)+string(rest), postResponseHookIDParam)

	tests := [][]byte{
		[]byte(`{bad`),
		[]byte(`{"jsonrpc":"2.0","method":"session/resume","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume"}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":[]}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":null}`),
	}
	for _, line := range tests {
		require.Equal(t, line, tagPostResponseHookRequest(line))
	}
}

func postResponseHookParams(values map[string]string, responseID string) json.RawMessage {
	params := map[string]string{postResponseHookIDParam: responseID}
	for key, value := range values {
		params[key] = value
	}

	raw, _ := json.Marshal(params)

	return raw
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
