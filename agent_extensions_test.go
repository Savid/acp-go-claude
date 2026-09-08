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
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestHandleForkSessionBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	agent := newForkTestAgent(t, nil)
	// Params this extension route cannot decode, and params that fail validation
	// as a whole, both name `params` itself rather than the Go decoder's prose.
	_, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{bad`))
	requireExactUnsupportedField(t, err, jsonFieldParams)

	raw, err := json.Marshal(acp.UnstableForkSessionRequest{})
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	requireExactUnsupportedField(t, err, jsonFieldParams)

	raw, err = json.Marshal(ForkSessionRequest("parent", "relative"))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	requireExactUnsupportedField(t, err, jsonFieldCwd)

	// The tombstoned parent still holds a LIVE registered instance — the exact
	// window where teardown is owed after the tombstone — so only the
	// tombstone fence, not the missing-store path, can refuse this fork.
	agent.mu.Lock()
	agent.deleted["tombstoned-parent"] = struct{}{}
	agent.sessions["tombstoned-parent"] = &agentSession{
		agent:           agent,
		id:              "tombstoned-parent",
		permissionRules: map[string]string{"Read": claude.BehaviorAllow},
	}
	agent.mu.Unlock()
	raw, err = json.Marshal(ForkSessionRequest("tombstoned-parent", cwd))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	requireUnknownSession(t, err)

	raw, err = json.Marshal(ForkSessionRequest("parent", cwd, WithSessionMCPServers(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}})))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.Error(t, err)

	raw = json.RawMessage(`{"sessionId":"parent","cwd":` + strconv.Quote(cwd) + `,"mcpServers":[{}]}`)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	var forkNameErr *acp.RequestError
	require.True(t, errors.As(err, &forkNameErr), "error = %T %[1]v", err)
	require.Equal(t, -32602, forkNameErr.Code)
	require.Equal(t, map[string]any{"mcpServers[0].name": valRequired}, forkNameErr.Data)

	previousStableMCPServers := stableMCPServers
	stableMCPServers = func([]acp.UnstableMcpServer) ([]acp.McpServer, error) {
		return nil, errors.New("stable conversion failed")
	}
	t.Cleanup(func() { stableMCPServers = previousStableMCPServers })
	raw, err = json.Marshal(ForkSessionRequest("parent", cwd))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	requireExactUnsupportedField(t, err, "mcpServers")
	stableMCPServers = func([]acp.UnstableMcpServer) ([]acp.McpServer, error) {
		return nil, &mapper.UnsupportedMCPServerError{Index: 2}
	}
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	requireExactUnsupportedField(t, err, "mcpServers[2]")
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
	requireAgentClosedRefusal(t, err)

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

	nativeParentID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	nativeHome := t.TempDir()
	nativePath := writeNativeTranscript(t, nativeHome, cwd, nativeParentID)
	nativeOnly := NewAgent(WithHome(nativeHome))
	nativeOnly.setConnection(newRecordingAgentClient())
	installFakeClaudeClient(nativeOnly, newFakeClaudeTransport())
	nativeRaw, err := json.Marshal(ForkSessionRequest(nativeParentID, cwd))
	require.NoError(t, err)
	_, err = nativeOnly.HandleExtensionMethod(ctx, ForkSessionMethod, nativeRaw)
	requireUnknownSession(t, err)
	require.NoFileExists(t, nativePath)
	require.NoError(t, nativeOnly.Close())

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
	_, err = emitFail.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.ErrorContains(t, err, "update failed")

	success := newForkTestAgent(t, nil)
	respAny, err := success.HandleExtensionMethod(ctx, ForkSessionMethod, raw)
	require.NoError(t, err)
	resp, ok := respAny.(acp.UnstableForkSessionResponse)
	require.True(t, ok)
	require.NotEmpty(t, resp.SessionId)
	require.NotEmpty(t, resp.ConfigOptions)
	require.Contains(t, success.sessions, resp.SessionId)

	storedParentID := acp.SessionId("22222222-2222-4222-8222-222222222222")
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(storedParentID)}, testStoredSessionEntries(t, ClaudeOptions{},
		[]byte(`{"type":"user","message":{"content":"stored parent"}}`),
	)))
	stored := NewAgent(WithHome(t.TempDir()), WithSessionStore(store))
	stored.setConnection(newRecordingAgentClient())
	installFakeClaudeClient(stored, newFakeClaudeTransport())
	storedRaw, err := json.Marshal(ForkSessionRequest(storedParentID, cwd))
	require.NoError(t, err)
	storedRespAny, err := stored.HandleExtensionMethod(ctx, ForkSessionMethod, storedRaw)
	require.NoError(t, err)
	storedResp, ok := storedRespAny.(acp.UnstableForkSessionResponse)
	require.True(t, ok)
	require.NotNil(t, stored.sessions[storedResp.SessionId].materialized)
	require.NoError(t, stored.Close())
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

func TestClosedAgentRejectsDirectNativeConstructionAdmissions(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, agent.Close())
	require.ErrorIs(t, agent.beginSessionConstruction(), errAgentClosed)

	_, err := agent.handleRateLimits(t.Context(), nil)
	requireAgentClosedRefusal(t, err)
	_, err = agent.startAndStoreSession(t.Context(), "closed", sessionStart{})
	require.ErrorIs(t, err, errAgentClosed)

	session := &agentSession{agent: agent, id: "closed", client: deadClaudeClient(t, nil), canRelaunch: true}
	require.ErrorIs(t, session.ensureClientAlive(t.Context()), errAgentClosed)
}
