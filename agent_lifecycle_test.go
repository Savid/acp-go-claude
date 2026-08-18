package claudeacp

import (
	"context"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func lifecycleOfferMeta(versions ...any) map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{"versions": versions}}
}

func lifecycleKeyMeta() map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1}}}
}

// TestLifecycleAnswerLandsOnTheResponseMeta pins the answer's placement: the
// response's own top-level `_meta`, never inside `agentCapabilities._meta`, and
// never under the vendor namespace.
func TestLifecycleAnswerLandsOnTheResponseMeta(t *testing.T) {
	t.Parallel()

	agent := NewAgent()

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
		Meta: lifecycleOfferMeta(1),
	})
	require.NoError(t, err)
	require.Contains(t, resp.Meta, lifecycle.MetaKey)

	answer := requireAnyMap(t, resp.Meta[lifecycle.MetaKey])
	require.Equal(t, []int{1}, answer["versions"])
	require.Equal(t, true, answer["updatesOutsidePrompt"])
	require.Equal(t, []string{}, answer["activityKinds"])

	capMeta := requireAnyMap(t, resp.AgentCapabilities.Meta)
	require.NotContains(t, capMeta, lifecycle.MetaKey)
	require.NotContains(t, requireAnyMap(t, capMeta[claudeMetaKey]), "lifecycle")
}

// TestLifecycleAnswerIsPerConfiguration pins the truth table against the same
// containment accessor that enforces the boundary: only the authoritative mode
// proves whole-tree vacancy, and no configuration claims a channel outside a
// prompt or an activity kind.
func TestLifecycleAnswerIsPerConfiguration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		mode          RuntimeContainmentMode
		authoritative bool
	}{
		{"authoritative", RuntimeContainmentAuthoritative, true},
		{"shared identity", RuntimeContainmentSharedIdentity, false},
		{"best effort", RuntimeContainmentBestEffort, false},
		{"unavailable", RuntimeContainmentUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			agent.containmentMode = tc.mode

			resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
				Meta: lifecycleOfferMeta(1),
			})
			require.NoError(t, err)

			answer := requireAnyMap(t, resp.Meta[lifecycle.MetaKey])
			require.Equal(t, tc.authoritative, answer["authoritativeQuiescence"])
			if tc.authoritative {
				require.Equal(t, "process-containment", answer["quiescenceSource"])
			} else {
				require.NotContains(t, answer, "quiescenceSource")
			}
			require.Equal(t, true, answer["updatesOutsidePrompt"])
			require.Equal(t, []string{}, answer["activityKinds"])

			require.Equal(t, tc.authoritative, agent.negotiatedLifecycle().AuthoritativeQuiescence)
		})
	}
}

// TestLifecycleKeyOmittedWithoutACommonVersion pins that no offer, and an offer
// with no shared version, both answer with the key omitted entirely rather than
// an empty answer.
func TestLifecycleKeyOmittedWithoutACommonVersion(t *testing.T) {
	t.Parallel()

	agent := NewAgent()

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotContains(t, resp.Meta, lifecycle.MetaKey)
	require.False(t, agent.negotiatedLifecycle().Present())

	resp, err = agent.Initialize(context.Background(), acp.InitializeRequest{
		Meta: lifecycleOfferMeta(2),
	})
	require.NoError(t, err)
	require.NotContains(t, resp.Meta, lifecycle.MetaKey)
	require.False(t, agent.negotiatedLifecycle().Present())
}

// TestLifecycleOfferIntersectsWithoutOrdering pins that only the answer is
// ordered: a host offering its versions out of order is intersected, not
// refused.
func TestLifecycleOfferIntersectsWithoutOrdering(t *testing.T) {
	t.Parallel()

	agent := NewAgent()

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{
		Meta: lifecycleOfferMeta(2, 1),
	})
	require.NoError(t, err)
	require.Equal(t, []int{1}, requireAnyMap(t, resp.Meta[lifecycle.MetaKey])["versions"])
}

// TestLifecycleOfferStrictness pins that a malformed offer is the one family
// literal this agent validates on initialize itself, and every refusal names
// the exact member path.
func TestLifecycleOfferStrictness(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{"non-object", map[string]any{lifecycle.MetaKey: []any{1}}, lifecycle.MetaPath},
		{"unknown member", map[string]any{lifecycle.MetaKey: map[string]any{
			"versions": []any{1}, "activityKinds": []any{},
		}}, lifecycle.MetaPath + ".activityKinds"},
		{"missing versions", map[string]any{lifecycle.MetaKey: map[string]any{}}, lifecycle.MetaPath + ".versions"},
		{"empty versions", lifecycleOfferMeta(), lifecycle.MetaPath + ".versions"},
		{"string version", lifecycleOfferMeta("1"), lifecycle.MetaPath + ".versions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()

			_, err := agent.Initialize(context.Background(), acp.InitializeRequest{Meta: tc.meta})
			requireRequestError(t, err, -32602, tc.field)
		})
	}
}

// TestLifecycleKeyRejectedOnNonCarryingSurfaces pins the closed surface table:
// every inbound surface outside initialize, session/prompt, and the outbound
// action surfaces rejects the family literal rather than ignoring it.
func TestLifecycleKeyRejectedOnNonCarryingSurfaces(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("session new", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session resume", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "s", Cwd: t.TempDir(), Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session load", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.LoadSession(ctx, acp.LoadSessionRequest{SessionId: "s", Cwd: t.TempDir(), Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session list", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session close", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "s", Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session delete", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{SessionId: "s", Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("session fork", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.HandleExtensionMethod(ctx, ForkSessionMethod, []byte(
			`{"sessionId":"s","cwd":"`+t.TempDir()+`","_meta":{"acp-go.dev/lifecycle":{"versions":[1]}}}`,
		))
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("authenticate", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.Authenticate(ctx, acp.AuthenticateRequest{MethodId: "claude", Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("logout", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.Logout(ctx, acp.LogoutRequest{Meta: lifecycleKeyMeta()})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("set config option value", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{SessionId: "s", ConfigId: configMode, Value: "default", Meta: lifecycleKeyMeta()},
		})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})

	t.Run("set config option boolean", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		_, err := agent.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: "s", ConfigId: configMode, Meta: lifecycleKeyMeta()},
		})
		requireRequestError(t, err, -32602, lifecycle.MetaPath)
	})
}

// TestLifecycleKeyOnCancelFailsClosedWireSilently pins that a cancel carrying
// the family literal is never applied: the refusal lands before the nonce gate
// and before any native interrupt, so the active turn keeps running.
func TestLifecycleKeyOnCancelFailsClosedWireSilently(t *testing.T) {
	t.Parallel()

	transport := newFakeClaudeTransport()
	agent := NewAgent()
	installFakeClaudeClient(agent, transport)
	agent.setConnection(newRecordingAgentClient())

	session := &agentSession{
		agent:  agent,
		id:     "session-1",
		client: claude.NewClient(nil, claude.Options{}, transport),
		turn:   make(chan struct{}, 1),
	}
	require.NoError(t, session.client.Start(context.Background()))
	agent.sessions[session.id] = session

	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	session.mu.Lock()
	session.cancel = cancel
	session.turnNonce = "nonce-1"
	session.mu.Unlock()

	err := agent.Cancel(context.Background(), acp.CancelNotification{
		SessionId: session.id,
		Meta:      lifecycleKeyMeta(),
	})
	requireRequestError(t, err, -32602, lifecycle.MetaPath)
	require.NoError(t, turnCtx.Err(), "a refused cancel never reaches the turn")
	require.Zero(t, transport.CloseCalls(), "a refused cancel never interrupts the native process")

	session.mu.Lock()
	session.cancel = nil
	session.mu.Unlock()
}

// requireRequestError asserts a JSON-RPC error with the code and the offending
// field named in its data.
func requireRequestError(t *testing.T, err error, code int, field string) {
	t.Helper()

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, code, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "error data names the offending field")
	require.Equal(t, field, data["field"])
}
