package claudeacp

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestNonInterruptibleLocalCarrierDeclinesLifecycle(t *testing.T) {
	agent := NewAgent()
	output := &bytes.Buffer{}
	conn := newLocalAgentConnection(agent, output, bytes.NewReader(nil))
	agent.setConnection(conn)

	resp, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.NotContains(t, resp.Meta, lifecycle.MetaKey)
	require.False(t, agent.negotiatedLifecycle().Present())
	require.NoError(t, conn.SessionUpdate(t.Context(), acp.SessionNotification{
		SessionId: "ordinary", Update: acp.UpdateAgentMessageText("ordinary"),
	}))
	require.Contains(t, output.String(), "ordinary")
	require.NoError(t, agent.Close())
}

func lifecycleOfferMeta(version any) map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{"version": version}}
}

func lifecycleKeyMeta() map[string]any {
	return lifecycleOfferMeta(1)
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
	require.Equal(t, 1, answer["version"])
	require.Equal(t, true, answer["updatesOutsidePrompt"])
	require.Equal(t, []string{}, answer["activityKinds"])

	capMeta := requireAnyMap(t, resp.AgentCapabilities.Meta)
	require.NotContains(t, capMeta, lifecycle.MetaKey)
	require.NotContains(t, requireAnyMap(t, capMeta[claudeMetaKey]), "lifecycle")
}

// TestLifecycleAnswerIsPerConfiguration pins the truth table against authority
// selection: a supplied host authority proves quiescence and ordinary execution
// does not.
func TestLifecycleAnswerIsPerConfiguration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		options       []Option
		authoritative bool
	}{
		{"host authority", []Option{WithHostAuthority(newFakeHostAuthority())}, true},
		{"ordinary", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(tc.options...)

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

func TestLifecycleKeyOmittedWithoutCapability(t *testing.T) {
	t.Parallel()

	agent := NewAgent()

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotContains(t, resp.Meta, lifecycle.MetaKey)
	require.False(t, agent.negotiatedLifecycle().Present())
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
			"version": 1, "activityKinds": []any{},
		}}, lifecycle.MetaPath + ".activityKinds"},
		{"missing version", map[string]any{lifecycle.MetaKey: map[string]any{}}, lifecycle.MetaPath + ".version"},
		{"other integer", lifecycleOfferMeta(2), lifecycle.MetaPath + ".version"},
		{"fractional version", lifecycleOfferMeta(1.5), lifecycle.MetaPath + ".version"},
		{"string version", lifecycleOfferMeta("1"), lifecycle.MetaPath + ".version"},
		{"boolean version", lifecycleOfferMeta(true), lifecycle.MetaPath + ".version"},
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
			`{"sessionId":"s","cwd":`+strconv.Quote(t.TempDir())+`,"_meta":{"acp-go.dev/lifecycle":{"version":1}}}`,
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
// the family literal is never applied: the refusal lands before any native
// interrupt, so the active turn keeps running.
//
// It also pins which of the two reserved objects decides the verdict. The route
// is the authenticator and is judged first, so a cancel that reaches the
// lifecycle refusal is one whose route already named the running turn, and a
// cancel carrying an invalid or absent route beside the same key reports the
// route instead. There is one verdict per frame, never a choice between two.
func TestLifecycleKeyOnCancelFailsClosedWireSilently(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{
			name:  "the current route reaches the reserved-key refusal",
			meta:  withLifecycleMeta(turnRouteMeta("nonce-1"), lifecycleKeyMeta()),
			field: lifecycle.MetaPath,
		},
		{
			name:  "a stale route outranks the reserved key",
			meta:  withLifecycleMeta(turnRouteMeta("nonce-0"), lifecycleKeyMeta()),
			field: routeMemberPath(routeFieldTurn),
		},
		{
			name:  "an absent route outranks the reserved key",
			meta:  lifecycleKeyMeta(),
			field: routeMetaPath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				Meta:      tc.meta,
			})
			requireRequestError(t, err, -32602, tc.field)
			require.NoError(t, turnCtx.Err(), "a refused cancel never reaches the turn")
			require.Zero(t, transport.CloseCalls(), "a refused cancel never interrupts the native process")

			session.mu.Lock()
			session.cancel = nil
			session.mu.Unlock()
		})
	}
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

// TestLifecycleKeyOnPromptYieldsToTheRouteVerdict pins the route-precedence rule
// on the other inbound surface that carries both reserved objects. One frame
// gets one verdict, and which of the two checks an implementation happens to run
// first must never decide it: the route is the authenticator, it is judged
// first, and a prompt carrying an invalid route beside the family key reports
// the route on every configuration.
//
// Both configurations are covered because the key means opposite things on them.
// On a connection that negotiated nothing the key is forbidden outright; on one
// that negotiated version 1 it is required and a malformed one is refused. The
// route outranks either refusal, and neither prompt reaches the harness.
func TestLifecycleKeyOnPromptYieldsToTheRouteVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		negotiated bool
		meta       map[string]any
		field      string
	}{
		{
			name:  "an unnegotiated connection refuses the key on its own",
			meta:  withLifecycleMeta(turnRouteMeta("nonce-1"), lifecycleKeyMeta()),
			field: lifecycle.MetaPath,
		},
		{
			name:  "an invalid route outranks the forbidden key",
			meta:  withLifecycleMeta(map[string]any{routeMetaKey: "bad"}, lifecycleKeyMeta()),
			field: routeMetaPath,
		},
		{
			name:       "a negotiated connection refuses a malformed correlation on its own",
			negotiated: true,
			meta: withLifecycleMeta(turnRouteMeta("nonce-1"), map[string]any{
				lifecycle.MetaKey: map[string]any{"version": 1, "unknown": true},
			}),
			field: lifecycle.MetaPath + ".unknown",
		},
		{
			name:       "an invalid route outranks the malformed correlation",
			negotiated: true,
			meta: withLifecycleMeta(map[string]any{routeMetaKey: "bad"}, map[string]any{
				lifecycle.MetaKey: map[string]any{"version": 1, "unknown": true},
			}),
			field: routeMetaPath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			session, transport, cleanup := newPromptFlowSession(t)
			defer cleanup()

			if tc.negotiated {
				_, err := session.agent.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
				require.NoError(t, err)
			}

			session.agent.sessions[session.id] = session

			_, err := session.agent.Prompt(ctx, acp.PromptRequest{
				SessionId: session.id,
				Meta:      tc.meta,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
			})
			requireRequestError(t, err, -32602, tc.field)
			for _, payload := range transport.Sent() {
				frame, isFrame := payload.(map[string]any)
				require.False(t, isFrame && frame["type"] == claude.MessageTypeUser,
					"a refused prompt writes no native frame")
			}
		})
	}
}

// promptCorrelationMeta stamps a route beside one lifecycle correlation value so
// a prompt reaches the correlation check instead of stopping at the route.
func promptCorrelationMeta(correlation any) map[string]any {
	return withLifecycleMeta(turnRouteMeta("nonce-1"), map[string]any{lifecycle.MetaKey: correlation})
}

// TestReservedPromptKeyRefusalShapes is the conformance battery over the two
// reserved keys `session/prompt` carries. The same six classes run against each
// key — absent, non-object, wrong version, empty identifier, over-bound
// identifier, unknown member — and each names the exact verdict and field path a
// host reads, so `missing` and `unsupported` can never be collapsed and a member
// refusal can never degrade to the bare path.
//
// Every case runs on a connection that negotiated the lifecycle capability,
// which is the only configuration where the lifecycle key is required rather
// than forbidden, and no case reaches the harness.
func TestReservedPromptKeyRefusalShapes(t *testing.T) {
	overBound := strings.Repeat("i", lifecycle.IdentifierBound+1)
	submission := func(members map[string]any) map[string]any {
		return map[string]any{"version": 1, "submission": members}
	}

	for _, tc := range []struct {
		name    string
		meta    map[string]any
		missing bool
		field   string
	}{
		// The route envelope.
		{
			name: "route absent",
			meta: map[string]any{
				lifecycle.MetaKey: submission(map[string]any{"submissionId": "s", "clientNonce": "c"}),
			},
			missing: true,
			field:   routeMetaPath,
		},
		{
			name:  "route non-object",
			meta:  map[string]any{routeMetaKey: "bad"},
			field: routeMetaPath,
		},
		{
			name:  "route wrong version",
			meta:  map[string]any{routeMetaKey: map[string]any{routeFieldVer: 2, routeFieldTurn: "nonce-1"}},
			field: routeMemberPath(routeFieldVer),
		},
		{
			name:  "route empty nonce",
			meta:  turnRouteMeta(""),
			field: routeMemberPath(routeFieldTurn),
		},
		{
			name:  "route over-bound nonce",
			meta:  turnRouteMeta(strings.Repeat("n", routeTurnNonceMaxBytes+1)),
			field: routeMemberPath(routeFieldTurn),
		},
		{
			name: "route unknown member",
			meta: map[string]any{routeMetaKey: map[string]any{
				routeFieldVer: 1, routeFieldTurn: "nonce-1", "sessionId": "s",
			}},
			field: routeMemberPath("sessionId"),
		},
		// The lifecycle prompt correlation.
		{
			name:    "lifecycle absent",
			meta:    turnRouteMeta("nonce-1"),
			missing: true,
			field:   lifecycle.MetaPath,
		},
		{
			name:  "lifecycle non-object",
			meta:  promptCorrelationMeta([]any{1}),
			field: lifecycle.MetaPath,
		},
		{
			name:  "lifecycle wrong version",
			meta:  promptCorrelationMeta(map[string]any{"version": 2, "submission": map[string]any{"submissionId": "s", "clientNonce": "c"}}),
			field: lifecycle.MetaPath + ".version",
		},
		{
			name:  "lifecycle empty identifier",
			meta:  promptCorrelationMeta(submission(map[string]any{"submissionId": "", "clientNonce": "c"})),
			field: lifecycle.MetaPath + ".submission.submissionId",
		},
		{
			name:  "lifecycle over-bound identifier",
			meta:  promptCorrelationMeta(submission(map[string]any{"submissionId": "s", "clientNonce": overBound})),
			field: lifecycle.MetaPath + ".submission.clientNonce",
		},
		{
			name:  "lifecycle unknown member",
			meta:  promptCorrelationMeta(map[string]any{"version": 1, "streamId": "s"}),
			field: lifecycle.MetaPath + ".streamId",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			session, transport, cleanup := newPromptFlowSession(t)
			defer cleanup()

			_, err := session.agent.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
			require.NoError(t, err)

			session.agent.sessions[session.id] = session

			_, err = session.agent.Prompt(ctx, acp.PromptRequest{
				SessionId: session.id,
				Meta:      tc.meta,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
			})

			if tc.missing {
				requireExactMissingField(t, err, tc.field)
			} else {
				requireExactUnsupportedField(t, err, tc.field)
			}

			for _, payload := range transport.Sent() {
				frame, isFrame := payload.(map[string]any)
				require.False(t, isFrame && frame["type"] == claude.MessageTypeUser,
					"a refused prompt writes no native frame")
			}
		})
	}
}
