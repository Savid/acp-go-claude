package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

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

func TestHandleRateLimits(t *testing.T) {
	ctx := context.Background()

	agent := NewAgent(
		WithHome(t.TempDir()),
		WithExecutablePath("/usr/bin/claude-test"),
		WithEnv(map[string]string{"CLAUDE_TEST": "1"}),
	)

	var gotOptions claude.Options

	reset := time.Date(2026, time.July, 9, 13, 40, 0, 0, time.UTC)
	agent.queryRateLimits = func(_ context.Context, options claude.Options) (claude.RateLimits, error) {
		gotOptions = options

		return claude.RateLimits{Windows: []claude.RateLimitWindow{
			{ID: sessionWindowID, UsedPercent: 92, ResetsAt: reset},
			{ID: "week-all-models", UsedPercent: 73.5},
		}}, nil
	}

	respAny, err := agent.HandleExtensionMethod(ctx, RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Equal(t, RateLimitsResponse{Windows: []RateLimitWindow{
		{ID: sessionWindowID, UsedPercent: 92, ResetsAt: "2026-07-09T13:40:00Z"},
		{ID: "week-all-models", UsedPercent: 73.5},
	}}, resp)

	require.Equal(t, "/usr/bin/claude-test", gotOptions.CLIPath)
	require.NotEmpty(t, gotOptions.ClaudeHome)
	require.Equal(t, map[string]string{"CLAUDE_TEST": "1"}, gotOptions.Env)

	encoded, err := json.Marshal(resp)
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"windows":[
			{"id":"session","usedPercent":92,"resetsAt":"2026-07-09T13:40:00Z"},
			{"id":"week-all-models","usedPercent":73.5}
		]}`,
		string(encoded),
	)

	respAny, err = agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`{}`))
	require.NoError(t, err)
	resp, ok = respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Len(t, resp.Windows, 2)

	respAny, err = agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`null`))
	require.NoError(t, err)
	resp, ok = respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Len(t, resp.Windows, 2)
}

const sessionWindowID = "session"

// emptyPanelAgent stubs the harness probe to report no windows, the shape a
// token-authenticated Claude home produces.
func emptyPanelAgent(t *testing.T, opts ...Option) *Agent {
	t.Helper()

	agent := NewAgent(append([]Option{WithHome(t.TempDir())}, opts...)...)
	agent.queryRateLimits = func(context.Context, claude.Options) (claude.RateLimits, error) {
		return claude.RateLimits{}, nil
	}

	// Never let a test reach the real Anthropic API.
	agent.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		return claude.RateLimits{}, nil
	}

	return agent
}

func TestHandleRateLimitsEmptyWindows(t *testing.T) {
	agent := emptyPanelAgent(t)

	respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.NotNil(t, resp.Windows)
	require.Empty(t, resp.Windows)

	encoded, err := json.Marshal(resp)
	require.NoError(t, err)
	require.JSONEq(t, `{"windows":[]}`, string(encoded))
}

func TestHandleRateLimitsFallsBackToAPI(t *testing.T) {
	agent := emptyPanelAgent(t, WithExecutablePath("/usr/bin/claude-test"), WithEnv(map[string]string{"K": "V"}))

	var gotProbe claude.RateLimitsProbe

	agent.queryRateLimitsAPI = func(_ context.Context, probe claude.RateLimitsProbe) (claude.RateLimits, error) {
		gotProbe = probe

		return claude.RateLimits{Windows: []claude.RateLimitWindow{
			{ID: sessionWindowID, UsedPercent: 6},
			{ID: "week-all-models", UsedPercent: 99},
		}}, nil
	}

	respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Equal(t, RateLimitsResponse{Windows: []RateLimitWindow{
		{ID: sessionWindowID, UsedPercent: 6},
		{ID: "week-all-models", UsedPercent: 99},
	}}, resp)

	// The probe sees the same executable, home, and env the CLI would launch with.
	require.Equal(t, "/usr/bin/claude-test", gotProbe.Options.CLIPath)
	require.NotEmpty(t, gotProbe.Options.ClaudeHome)
	require.Equal(t, map[string]string{"K": "V"}, gotProbe.Options.Env)
	require.Equal(t, "acp-go-claude/0.1.0", gotProbe.UserAgent)
}

// A non-empty harness panel is authoritative; the adapter must not call out.
func TestHandleRateLimitsSkipsAPIWhenPanelReportsWindows(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()))
	agent.queryRateLimits = func(context.Context, claude.Options) (claude.RateLimits, error) {
		return claude.RateLimits{Windows: []claude.RateLimitWindow{{ID: sessionWindowID, UsedPercent: 6}}}, nil
	}
	agent.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		t.Fatal("direct API probe must not run when the harness reports windows")

		return claude.RateLimits{}, nil
	}

	respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Len(t, resp.Windows, 1)
}

func TestHandleRateLimitsDirectAPIDisabled(t *testing.T) {
	agent := emptyPanelAgent(t, WithClaudeDirectAPI(false))
	agent.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		t.Fatal("direct API probe must not run when DirectAPI is disabled")

		return claude.RateLimits{}, nil
	}

	respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Empty(t, resp.Windows)
}

// The API fallback can bill an inference request, so repeated polls inside the
// TTL must reuse the memoized result.
func TestHandleRateLimitsAPIResultIsCached(t *testing.T) {
	agent := emptyPanelAgent(t)

	calls := 0
	agent.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		calls++

		return claude.RateLimits{Windows: []claude.RateLimitWindow{{ID: sessionWindowID, UsedPercent: 6}}}, nil
	}

	for range 3 {
		respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
		require.NoError(t, err)

		resp, ok := respAny.(RateLimitsResponse)
		require.True(t, ok)
		require.Len(t, resp.Windows, 1)
	}

	require.Equal(t, 1, calls)

	// An expired entry is refetched rather than served stale.
	agent.rateLimitsCache.fetched = time.Now().Add(-2 * rateLimitsAPITTL)

	_, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

// A failing probe degrades to empty windows: the same answer the harness gives,
// rather than failing a request that only asked what the quota looks like.
func TestHandleRateLimitsAPIFailureDegradesToEmptyWindows(t *testing.T) {
	agent := emptyPanelAgent(t)

	calls := 0
	agent.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		calls++

		return claude.RateLimits{}, errors.New("probe exploded")
	}

	respAny, err := agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)

	resp, ok := respAny.(RateLimitsResponse)
	require.True(t, ok)
	require.Empty(t, resp.Windows)

	// A failure is not cached, so the next poll retries.
	_, err = agent.HandleExtensionMethod(context.Background(), RateLimitsMethod, nil)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestHandleRateLimitsErrors(t *testing.T) {
	ctx := context.Background()

	agent := NewAgent(WithHome(t.TempDir()))
	agent.queryRateLimits = func(context.Context, claude.Options) (claude.RateLimits, error) {
		return claude.RateLimits{}, errors.New("probe failed")
	}

	_, err := agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`{"unexpected":1}`))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`{bad`))
	require.Error(t, err)

	// A failed usage probe degrades to empty windows instead of failing.
	result, err := agent.HandleExtensionMethod(ctx, RateLimitsMethod, nil)
	require.NoError(t, err)
	resp, ok := result.(RateLimitsResponse)
	require.True(t, ok)
	require.Empty(t, resp.Windows)

	// An unresolvable Claude home degrades the same way a failed probe does:
	// empty windows, no error, and neither probe runs — there is nothing to
	// probe without a resolved home.
	invalidHome := NewAgent(WithHome(string([]byte{0})))
	invalidHome.queryRateLimits = func(context.Context, claude.Options) (claude.RateLimits, error) {
		t.Fatal("usage probe must not run when the Claude home cannot be resolved")

		return claude.RateLimits{}, nil
	}
	invalidHome.queryRateLimitsAPI = func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error) {
		t.Fatal("direct API probe must not run when the Claude home cannot be resolved")

		return claude.RateLimits{}, nil
	}
	result, err = invalidHome.HandleExtensionMethod(ctx, RateLimitsMethod, nil)
	require.NoError(t, err)
	resp, ok = result.(RateLimitsResponse)
	require.True(t, ok)
	require.NotNil(t, resp.Windows)
	require.Empty(t, resp.Windows)

	encoded, err := json.Marshal(resp)
	require.NoError(t, err)
	require.JSONEq(t, `{"windows":[]}`, string(encoded))

	closed := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, closed.Close())
	_, err = closed.HandleExtensionMethod(ctx, RateLimitsMethod, nil)
	require.ErrorIs(t, err, errAgentClosed)
}
