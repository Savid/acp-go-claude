package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestHandleForkSessionBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	agent := newForkTestAgent(t, nil)
	_, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{bad`))
	require.Error(t, err)

	raw, err := json.Marshal(acp.UnstableForkSessionRequest{})
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.Error(t, err)

	raw, err = json.Marshal(ForkSessionRequest("parent", "relative"))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorContains(t, err, "absolute")

	raw, err = json.Marshal(ForkSessionRequest("parent", cwd, WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.Error(t, err)

	raw = json.RawMessage(`{"sessionId":"parent","cwd":` + strconv.Quote(cwd) + `,"mcpServers":[{}]}`)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	var forkNameErr *acp.RequestError
	require.True(t, errors.As(err, &forkNameErr), "error = %T %[1]v", err)
	require.Equal(t, -32602, forkNameErr.Code)
	require.Equal(t, map[string]any{"mcpServers[0].name": validationRequired}, forkNameErr.Data)

	previousStableMCPServers := stableMCPServers
	stableMCPServers = func([]acp.UnstableMcpServer) ([]acp.McpServer, error) {
		return nil, errors.New("stable conversion failed")
	}
	t.Cleanup(func() { stableMCPServers = previousStableMCPServers })
	raw, err = json.Marshal(ForkSessionRequest("parent", cwd))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorContains(t, err, "stable conversion failed")
	stableMCPServers = previousStableMCPServers

	previousUUIDRandom := uuidRandom
	uuidRandom = bytes.NewBuffer(nil)
	t.Cleanup(func() { uuidRandom = previousUUIDRandom })
	raw, err = json.Marshal(ForkSessionRequest("parent", cwd))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorContains(t, err, "read random uuid")
	uuidRandom = previousUUIDRandom

	closed := newForkTestAgent(t, nil)
	closed.closed = true
	raw, err = json.Marshal(ForkSessionRequest("parent", cwd))
	require.NoError(t, err)
	_, err = closed.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorIs(t, err, errAgentClosed)

	permissionLoadErr := NewAgent(WithHome(string([]byte{0})))
	permissionLoadErr.setConnection(newRecordingAgentClient())
	installFakeClaudeClient(permissionLoadErr, newFakeClaudeTransport())
	_, err = permissionLoadErr.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorContains(t, err, "load permission rules")

	// A generic native start failure surfaces verbatim.
	startErr := errors.New("start failed")
	startFail := newForkTestAgent(t, func() *fakeClaudeTransport {
		transport := newFakeClaudeTransport()
		transport.startErr = startErr

		return transport
	})
	_, err = startFail.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorIs(t, err, startErr)

	// Forking an unknown or deleted parent returns the uniform unknown-session
	// invalid-params error, matching resume and load — not a raw -32603.
	missingParent := newForkTestAgent(t, func() *fakeClaudeTransport {
		transport := newFakeClaudeTransport()
		transport.startErr = claude.ErrSessionNotFound

		return transport
	})
	_, err = missingParent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	var missingReqErr *acp.RequestError
	require.ErrorAs(t, err, &missingReqErr)
	require.Equal(t, -32602, missingReqErr.Code)
	missingData, ok := missingReqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unknown session", missingData[jsonFieldError])
	require.Equal(t, acpFieldSessionID, missingData[jsonFieldField])

	limit := NewAgent(WithHome(t.TempDir()), WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	limit.setConnection(newRecordingAgentClient())
	limit.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, newFakeClaudeTransport())
	}
	limit.sessions["parent"] = &agentSession{agent: limit, id: "parent", permissionRules: map[string]string{"Read": claude.BehaviorAllow}}
	_, err = limit.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.Error(t, err)

	emitFail := newForkTestAgent(t, nil)
	emitConn, ok := emitFail.connection().(*recordingAgentClient)
	require.True(t, ok)
	emitConn.sessionUpdateErr = errors.New("update failed")
	respAny, err := emitFail.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.NoError(t, err)
	resp, ok := respAny.(acp.UnstableForkSessionResponse)
	require.True(t, ok)
	require.NotEmpty(t, resp.SessionId)

	success := newForkTestAgent(t, nil)
	respAny, err = success.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.NoError(t, err)
	resp, ok = respAny.(acp.UnstableForkSessionResponse)
	require.True(t, ok)
	require.NotEmpty(t, resp.SessionId)
	require.NotEmpty(t, resp.ConfigOptions)
	require.Contains(t, success.sessions, resp.SessionId)
}

func newForkTestAgent(t *testing.T, transportFactory func() *fakeClaudeTransport) *Agent {
	t.Helper()

	if transportFactory == nil {
		transportFactory = newFakeClaudeTransport
	}

	agent := NewAgent(WithHome(t.TempDir()))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, transportFactory())
	}
	agent.sessions["parent"] = &agentSession{
		agent:           agent,
		id:              "parent",
		permissionRules: map[string]string{"Read": claude.BehaviorAllow},
	}

	return agent
}
