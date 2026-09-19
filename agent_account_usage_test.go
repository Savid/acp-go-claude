package claudeacp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
)

// accountUsageSessionField is the request member naming the session.
const accountUsageSessionField = "sessionId"

// limitKindSession is the id of the five-hour window.
const limitKindSession = "session"

func callAccountUsage(t *testing.T, h *harness, params any) (wire.AccountUsageResponse, error) {
	t.Helper()

	raw, err := h.conn.CallExtension(h.ctx(), AccountUsageMethod, params)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	var response wire.AccountUsageResponse

	require.NoError(t, json.Unmarshal(raw, &response))
	require.NoError(t, response.Validate())

	return response, nil
}

// The read launches the session's process when none is live, reuses it when
// there is one, and maps every native window, including the model-scoped one,
// in native order.
func TestAccountUsageReadsThroughTheSession(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	sessionID := h.newSession().SessionId

	before := time.Now()
	response, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err)

	observed, parseErr := time.Parse(time.RFC3339, response.Limits[0].ObservedAt)
	require.NoError(t, parseErr)
	require.False(t, observed.Before(before.Truncate(time.Second)))

	for i := range response.Limits {
		response.Limits[i].ObservedAt = ""
		response.Limits[i].StaleAt = ""
	}
	require.Equal(t, wire.AccountUsageResponse{Available: true, Plan: "enterprise", Limits: []wire.AccountUsageLimit{
		{ID: limitKindSession, UsedPercent: 4, ResetsAt: "2026-09-17T03:30:00Z"},
		{ID: "weekly_all", UsedPercent: 15, ResetsAt: "2026-09-19T08:00:00Z"},
		{ID: "weekly_scoped/Fable", Label: "Fable", UsedPercent: 22, ResetsAt: "2026-09-19T08:00:00Z"},
	}}, response)

	again, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err, "a second read reuses the live process")
	require.Len(t, again.Limits, 3)
	require.Nil(t, again.UsageAllowed, "claude makes no statement about ordinary usage, so none is derived")
}

func TestAccountUsageRefusals(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	sessionID := h.newSession().SessionId

	cases := []struct {
		name   string
		params any
		data   map[string]any
	}{
		{"missing session", map[string]any{}, map[string]any{"error": "missing", "field": accountUsageSessionField}},
		{"unknown session", map[string]any{accountUsageSessionField: "nope"}, map[string]any{"error": "unknown session", "field": accountUsageSessionField}},
		{"unadvertised provider", map[string]any{accountUsageSessionField: sessionID, "providerId": "anthropic"}, map[string]any{"error": "unsupported", "field": "providerId"}},
		{"lifecycle key", map[string]any{accountUsageSessionField: sessionID, "_meta": map[string]any{wire.LifecycleKey: map[string]any{}}}, map[string]any{"error": "unsupported", "field": `_meta["` + wire.LifecycleKey + `"]`}},
	}

	for _, tc := range cases {
		_, err := callAccountUsage(t, h, tc.params)
		require.Equal(t, -32602, requestErrorCode(t, err), tc.name)
		require.Equal(t, tc.data, requestErrorData(t, err), tc.name)
	}

	_, err := h.conn.UnstableDeleteSession(h.ctx(), wire.DeleteSessionRequest(sessionID))
	require.NoError(t, err)

	_, err = callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.Equal(t, -32602, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "unknown session", "field": accountUsageSessionField}, requestErrorData(t, err), "a tombstoned session")
}

// A home whose account reports no allowance answers not_reported; a native
// control refusal is the account_usage failure class.
func TestAccountUsageUnavailableAndRefused(t *testing.T) {
	t.Parallel()

	unavailable := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvAccountUsage: "unavailable"}))
	unavailable.initialize()

	response, err := callAccountUsage(t, unavailable, map[string]any{accountUsageSessionField: unavailable.newSession().SessionId})
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageUnavailable(wire.AccountUsageNotReported), response)

	refused := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvAccountUsage: "refuse"}))
	refused.initialize()

	_, err = callAccountUsage(t, refused, map[string]any{accountUsageSessionField: refused.newSession().SessionId})
	require.Equal(t, -32603, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "claude_internal_failure", "class": "account_usage"}, requestErrorData(t, err))
}

func TestAccountUsageResponseMapping(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 17, 2, 41, 3, 900, time.UTC)
	scoped := claude.ScopedWindow{Window: claude.Window{Utilization: new(float64(22))}, DisplayName: "Fable"}

	response, err := accountUsageResponse(claude.AccountUsage{Plan: "pro", Available: true, Windows: &claude.RateLimits{ModelScoped: []claude.ScopedWindow{scoped}}}, now)
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageResponse{Available: true, Plan: "pro", Limits: []wire.AccountUsageLimit{{ObservedAt: "2026-09-17T02:41:03Z", StaleAt: "2026-09-17T02:42:03Z", ID: "weekly_scoped/Fable", Label: "Fable", UsedPercent: 22}}}, response, "the display name keys and labels a scoped limit")

	padded := claude.ScopedWindow{Window: claude.Window{Utilization: new(float64(22)), ResetsAt: "2026-09-19T08:00:00.051625+00:00"}, DisplayName: " Fable "}
	response, err = accountUsageResponse(claude.AccountUsage{Plan: " Max ", Available: true, Windows: &claude.RateLimits{ModelScoped: []claude.ScopedWindow{padded}}}, now)
	require.NoError(t, err)
	require.Equal(t, wire.AccountUsageResponse{Available: true, Plan: "Max", Limits: []wire.AccountUsageLimit{{ObservedAt: "2026-09-17T02:41:03Z", StaleAt: "2026-09-17T02:42:03Z", ID: "weekly_scoped/Fable", Label: "Fable", UsedPercent: 22, ResetsAt: "2026-09-19T08:00:00Z"}}}, response, "native padding is trimmed from the plan and the model name")

	for name, usage := range map[string]claude.AccountUsage{
		"not available": {Available: false, Windows: &claude.RateLimits{ModelScoped: []claude.ScopedWindow{scoped}}},
		"no windows":    {Available: true},
		"empty report":  {Available: true, Windows: &claude.RateLimits{}},
		"no percentage": {Available: true, Windows: &claude.RateLimits{Session: &claude.Window{ResetsAt: "2026-09-19T08:00:00Z"}}},
	} {
		response, err := accountUsageResponse(usage, now)
		require.NoError(t, err, name)
		require.Equal(t, wire.AccountUsageUnavailable(wire.AccountUsageNotReported), response, name)
	}

	for name, limits := range map[string]claude.RateLimits{
		"bad reset":      {Session: &claude.Window{Utilization: new(float64(1)), ResetsAt: "tomorrow"}},
		"repeated model": {ModelScoped: []claude.ScopedWindow{scoped, scoped}},
		"negative usage": {Session: &claude.Window{Utilization: new(float64(-1))}},
	} {
		_, err := accountUsageResponse(claude.AccountUsage{Available: true, Windows: &limits}, now)
		require.Error(t, err, name)
	}
}

// A read while a prompt holds the session's foreground is refused with the
// prompt backpressure token and succeeds once the prompt has settled.
func TestAccountUsageRefusedWhilePromptHoldsTheGate(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	sessionID := h.newSession().SessionId

	done := make(chan error, 1)

	go func() {
		_, err := h.prompt(sessionID, "BLOCK", promptMeta(1))
		done <- err
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 3 })

	_, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.Equal(t, -32600, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "backpressure", "limit": limitSessionPrompt}, requestErrorData(t, err))

	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(sessionID)))
	require.NoError(t, <-done)

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		kinds := eventTypes(lifecycleEvents(updates))

		return len(kinds) > 0 && kinds[len(kinds)-1] == "state_update:idle"
	})

	response, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err)
	require.Len(t, response.Limits, 3)
}

// A closed session answers unknown session, the same as a deleted one.
func TestAccountUsageAfterCloseIsUnknownSession(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	sessionID := h.newSession().SessionId

	_, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err)

	_, err = callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.Equal(t, -32602, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "unknown session", "field": accountUsageSessionField}, requestErrorData(t, err))
}

// A read on a session whose process has exited launches a new one, and that
// launch publishes the incarnation's opening updates as every launch does.
func TestAccountUsageColdReadPublishesOpeningUpdates(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	sessionID := h.newSession().SessionId

	_, err := h.prompt(sessionID, "CRASH", promptMeta(1))
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])

	before := len(h.rec.snapshot())

	response, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
	require.NoError(t, err)
	require.True(t, response.Available)

	var kinds []string

	for _, update := range h.rec.snapshot()[before:] {
		switch {
		case update.Update.SessionInfoUpdate != nil:
			kinds = append(kinds, "session_info_update")
		case update.Update.AvailableCommandsUpdate != nil:
			kinds = append(kinds, "available_commands_update")
		}
	}

	require.Contains(t, kinds, "session_info_update")
	require.Contains(t, kinds, "available_commands_update")
	require.Equal(t, 2, lifecycleIncarnations(h.rec.snapshot()), "the relaunch opens a new incarnation")
}

// The request's trace keys open the read's span, as on every other request.
func TestAccountUsagePropagatesTraceContext(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	h := newHarness(t, WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))))
	h.initialize()
	sessionID := h.newSession().SessionId

	const traceID = "0af7651916cd43dd8448eb211c80319c"

	_, err := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID, "_meta": map[string]any{"traceparent": "00-" + traceID + "-b7ad6b7169203331-01"}})
	require.NoError(t, err)

	var traced bool

	for _, span := range exporter.GetSpans() {
		traced = traced || span.SpanContext.TraceID().String() == traceID
	}

	require.True(t, traced, "the read's span continues the host's trace")
}

// An unknown extension method is still method-not-found beside the served one.
func TestAccountUsageLeavesOtherExtensionsUnserved(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()

	_, err := h.conn.CallExtension(h.ctx(), "_claude/accountUsages", map[string]any{})
	require.Equal(t, -32601, requestErrorCode(t, err))
	require.Equal(t, "_claude/accountUsages", requestErrorData(t, err)["method"])
}

// A read that holds the session's foreground refuses a concurrent prompt with
// the prompt backpressure token, and a close that lands under a held read ends
// it with the session's own answer rather than an internal failure.
func TestPromptRefusedWhileReadHoldsTheGate(t *testing.T) {
	t.Parallel()

	hold := filepath.Join(t.TempDir(), "usage-held")
	h := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvAccountUsage: "hold", fakeClaudeEnvAccountUsageHold: hold}))
	h.initialize(withLifecycle())
	sessionID := h.newSession().SessionId

	held := func() bool {
		_, statErr := os.Stat(hold)

		return statErr == nil
	}

	done := make(chan error, 1)

	go func() {
		_, readErr := callAccountUsage(t, h, map[string]any{accountUsageSessionField: sessionID})
		done <- readErr
	}()

	require.Eventually(t, held, 5*time.Second, 10*time.Millisecond)

	_, err := h.prompt(sessionID, "HELLO", promptMeta(1))
	require.Equal(t, -32600, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "backpressure", "limit": limitSessionPrompt}, requestErrorData(t, err))

	require.NoError(t, os.Remove(hold))
	require.NoError(t, <-done)

	go func() {
		_, readErr := h.conn.CallExtension(h.ctx(), AccountUsageMethod, map[string]any{accountUsageSessionField: sessionID})
		done <- readErr
	}()

	require.Eventually(t, held, 5*time.Second, 10*time.Millisecond)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err)

	err = <-done
	require.Equal(t, -32602, requestErrorCode(t, err))
	require.Equal(t, map[string]any{"error": "unknown session", "field": accountUsageSessionField}, requestErrorData(t, err))
}
