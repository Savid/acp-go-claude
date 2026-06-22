package claudeacp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// rawForwardACPClient is a fake ACP client that records and answers the
// requests forwarded by localAgentConnection's client methods. It is shared by
// the dispatcher tests here and by agent_test.go.
type rawForwardACPClient struct {
	mu               sync.Mutex
	mcpNotifications int
	extensions       []string
	elicitation      map[string]any
}

func (c *rawForwardACPClient) handle(
	_ context.Context,
	method string,
	params json.RawMessage,
) (any, *acp.RequestError) {
	switch method {
	case "_claude/sdkMessage":
		c.mu.Lock()
		c.extensions = append(c.extensions, string(params))
		c.mu.Unlock()

		return nil, nil
	case acp.ClientMethodElicitationComplete:
		return nil, nil
	case acp.ClientMethodElicitationCreate:
		var payload map[string]any
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.mu.Lock()
		c.elicitation = payload
		c.mu.Unlock()

		return acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{"ok": true}},
		}, nil
	case acp.ClientMethodFsReadTextFile:
		return acp.ReadTextFileResponse{Content: "file"}, nil
	case acp.ClientMethodFsWriteTextFile:
		return acp.WriteTextFileResponse{}, nil
	case acp.ClientMethodMcpConnect:
		return acp.UnstableConnectMcpResponse{ConnectionId: "conn-1"}, nil
	case acp.ClientMethodMcpDisconnect:
		return acp.UnstableDisconnectMcpResponse{}, nil
	case acp.ClientMethodMcpMessage:
		var request acp.UnstableMessageMcpRequest
		if err := json.Unmarshal(params, &request); err == nil && isMCPNotificationMethod(request.Method) {
			c.mu.Lock()
			c.mcpNotifications++
			c.mu.Unlock()

			return nil, nil
		}

		return map[string]any{"ok": true}, nil
	case acp.ClientMethodSessionRequestPermission:
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected("allow_once"),
		}, nil
	case acp.ClientMethodSessionUpdate:
		return nil, nil
	case acp.ClientMethodTerminalCreate:
		return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
	case acp.ClientMethodTerminalKill:
		return acp.KillTerminalResponse{}, nil
	case acp.ClientMethodTerminalOutput:
		return acp.TerminalOutputResponse{Output: "out"}, nil
	case acp.ClientMethodTerminalRelease:
		return acp.ReleaseTerminalResponse{}, nil
	case acp.ClientMethodTerminalWaitForExit:
		exitCode := 7

		return acp.WaitForTerminalExitResponse{ExitCode: &exitCode}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func (c *rawForwardACPClient) extensionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.extensions)
}

func (c *rawForwardACPClient) notificationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.mcpNotifications
}

func TestLocalAgentClientForwardsMethods(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	rawClient := &rawForwardACPClient{}
	_ = connectAgentRawForTest(t, agent, rawClient.handle)
	conn := agent.connection()
	ctx := context.Background()

	select {
	case <-conn.Done():
	default:
	}

	read, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "/tmp/a"})
	require.NoError(t, err)
	require.Equal(t, "file", read.Content)

	_, err = conn.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "/tmp/a", Content: "x"})
	require.NoError(t, err)

	err = conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "elicit-1"})
	require.NoError(t, err)

	connected, err := conn.UnstableConnectMcp(ctx, acp.UnstableConnectMcpRequest{AcpId: "server-1"})
	require.NoError(t, err)
	require.Equal(t, acp.UnstableMcpConnectionId("conn-1"), connected.ConnectionId)

	result, err := conn.UnstableMessageMcp(ctx, acp.UnstableMessageMcpRequest{
		ConnectionId: "conn-1",
		Method:       "tools/list",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"ok": true}, result)

	err = conn.UnstableNotifyMcp(ctx, acp.UnstableMessageMcpNotification{
		ConnectionId: "conn-1",
		Method:       "notifications/progress",
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rawClient.notificationCount() == 1 }, time.Second, 10*time.Millisecond)

	_, err = conn.UnstableDisconnectMcp(ctx, acp.UnstableDisconnectMcpRequest{ConnectionId: "conn-1"})
	require.NoError(t, err)

	permission, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: "allow_once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	require.NoError(t, err)
	require.NotNil(t, permission.Outcome.Selected)

	err = conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: "session-1",
		Update:    acp.UpdateAgentMessageText("hello"),
	})
	require.NoError(t, err)

	elicitation, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Approve?",
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Type: acp.UnstableElicitationSchemaTypeObject,
			},
		},
	}, elicitationScope{SessionID: "session-1", ToolCallID: "tool-1"})
	require.NoError(t, err)
	require.NotNil(t, elicitation.Accept)
	require.Equal(t, true, elicitation.Accept.Content["ok"])

	elicitation, err = conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Approve?",
			Mode:    "form",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, elicitation.Accept)

	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{}, elicitationScope{})
	require.Error(t, err)

	created, err := conn.CreateTerminal(ctx, acp.CreateTerminalRequest{})
	require.NoError(t, err)
	require.Equal(t, "terminal-1", created.TerminalId)

	_, err = conn.KillTerminal(ctx, acp.KillTerminalRequest{TerminalId: "terminal-1"})
	require.NoError(t, err)

	output, err := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{TerminalId: "terminal-1"})
	require.NoError(t, err)
	require.Equal(t, "out", output.Output)

	_, err = conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{TerminalId: "terminal-1"})
	require.NoError(t, err)

	exited, err := conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{TerminalId: "terminal-1"})
	require.NoError(t, err)
	require.NotNil(t, exited.ExitCode)
	require.Equal(t, 7, *exited.ExitCode)

	err = conn.NotifyExtension(ctx, "_claude/sdkMessage", map[string]any{"ok": true})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rawClient.extensionCount() == 1 }, time.Second, 10*time.Millisecond)

	err = conn.NotifyExtension(ctx, "not-extension", nil)
	require.Error(t, err)
}

func TestLocalAgentConnectionHandleBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	conn := &localAgentConnection{agent: agent}
	ctx := context.Background()

	_, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{`))
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionPrompt, json.RawMessage(`{"sessionId":"missing","prompt":null}`))
	require.Equal(t, -32600, reqErr.Code)

	result, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{"protocolVersion":1}`))
	require.Nil(t, reqErr)
	require.IsType(t, acp.InitializeResponse{}, result)

	_, reqErr = conn.handle(ctx, "missing", nil)
	require.Equal(t, -32601, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodProvidersList, nil)
	require.Equal(t, -32601, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodProvidersSet, nil)
	require.Equal(t, -32601, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodProvidersDisable, nil)
	require.Equal(t, -32601, reqErr.Code)

	_, reqErr = conn.handle(ctx, "_example/unknown", nil)
	require.Equal(t, -32601, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionPrompt, json.RawMessage(`{"sessionId":"missing","prompt":null}`))
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionPrompt, json.RawMessage(`{"sessionId":"missing","prompt":[]}`))
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodDocumentDidOpen, json.RawMessage(`{`))
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionCancel, json.RawMessage(`{"sessionId":"missing"}`))
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = conn.handle(ctx, acp.AgentMethodMcpMessage, json.RawMessage(`{`))
	require.Equal(t, -32602, reqErr.Code)

	agent.sessions["session-1"] = &Session{}
	result, reqErr = conn.handle(ctx, acp.AgentMethodDocumentDidOpen, json.RawMessage(
		`{"sessionId":"session-1","uri":"file:///tmp/a.go","languageId":"go","text":"package a","version":1}`,
	))
	require.Nil(t, reqErr)
	require.Nil(t, result)
}
