package claudeacp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestTurnRouteContextStampsNotifications(t *testing.T) {
	ctx := withTurnRoute(context.Background(), "turn-old")
	require.Equal(t, turnRouteMeta("turn-old"), turnRouteMetaFromContext(ctx))
	require.Nil(t, turnRouteMetaFromContext(context.Background()))
	var nilContext context.Context
	require.Nil(t, turnRouteMetaFromContext(nilContext))
}

func TestControlCallbackOwnerEdgeStates(t *testing.T) {
	require.Nil(t, controlCallbackAdmissionFromContext(context.TODO()))
	var nilContext context.Context
	require.Nil(t, controlCallbackAdmissionFromContext(nilContext))

	session := &agentSession{agent: NewAgent()}
	_, live := session.callbackOwner(nil, "")
	require.False(t, live)

	session.closing = true
	_, live = session.callbackOwner(nil, "route")
	require.False(t, live)
	session.closing = false

	_, cancel := context.WithCancel(t.Context())
	session.cancel = cancel
	session.turnNonce = "prompt-route"
	owner, live := session.callbackOwner(nil, "prompt-route")
	require.True(t, live)
	require.Nil(t, owner.incarnation)
	cancel()

	incarnation := &nativeIncarnation{}
	session.setAutonomousRoute("autonomous-route", incarnation)
	owner, live = session.callbackOwner(nil, "autonomous-route")
	require.True(t, live)
	require.Same(t, incarnation, owner.incarnation)
	require.True(t, owner.autonomous)

	incarnation.failed.Store(true)
	_, live = session.callbackOwner(nil, "autonomous-route")
	require.False(t, live)

	callbackCtx, finish, admitted := session.admitControlCallback(t.Context(), "autonomous-route")
	require.False(t, admitted)
	require.Equal(t, t.Context(), callbackCtx)
	finish()
	session.stopNativePump()
}

func TestControlCallbackAdmissionRechecksTheServingIncarnation(t *testing.T) {
	session, _, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))

	incarnation := &nativeIncarnation{}
	session.setAutonomousRoute("retired-route", incarnation)
	turnID, err := stream.openAgentTurn(t.Context(), "retired-route")
	require.NoError(t, err)
	require.NotEmpty(t, turnID)

	_, finish, admitted := session.admitControlCallback(t.Context(), "retired-route")
	require.False(t, admitted)
	finish()
}

func TestFullClientCallCapacityRejectsBeforeRouteRotation(t *testing.T) {
	session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	session.agent.options.ConcurrencyLimits.MaxConcurrentClientCalls = 1
	require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))

	route := session.autonomousRoute()
	incarnation := session.currentNativeIncarnation()
	callbackCtx, finishA, admitted := session.admitControlCallback(t.Context(), route)
	require.True(t, admitted)

	_, finishB, accepted := session.admitControlCallback(t.Context(), route)
	require.False(t, accepted, "a full client-call capacity returns backpressure without waiting")
	finishB()

	require.True(t, session.rotateAutonomousRoute(incarnation, "successor-route"))
	_, active := session.activeControlCallbackContext(callbackCtx)
	require.False(t, active)
	finishA()
	_, active = session.activeControlCallbackContext(callbackCtx)
	require.False(t, active)
}

func TestRouteEnvelopeHardCutover(t *testing.T) {
	route, err := parseInboundTurnRoute(turnRouteMeta("turn-1"))
	require.NoError(t, err)
	require.Equal(t, "turn-1", route.turnNonce)
	decoded, err := parseInboundTurnRoute(map[string]any{routeMetaKey: map[string]any{
		routeFieldVer: float64(1), routeFieldTurn: "decoded-turn",
	}})
	require.NoError(t, err)
	require.Equal(t, "decoded-turn", decoded.turnNonce)
	boundaryNonce := strings.Repeat("n", routeTurnNonceMaxBytes)
	boundary, err := parseInboundTurnRoute(turnRouteMeta(boundaryNonce))
	require.NoError(t, err)
	require.Equal(t, boundaryNonce, boundary.turnNonce)
	require.False(t, routeVersionIsOne("1"))

	generated, err := stampRouteMeta(nil, elicitationScope{SessionID: "s", TurnNonce: "t"})
	require.NoError(t, err)
	generatedRoute, ok := generated[routeMetaKey].(map[string]any)
	require.True(t, ok)
	generatedRequestID, ok := generatedRoute["requestId"].(string)
	require.True(t, ok)
	require.Len(t, generatedRequestID, 32)

	for _, meta := range []map[string]any{
		nil,
		{routeMetaKey: "bad"},
		{routeMetaKey: map[string]any{routeFieldVer: 2, routeFieldTurn: "turn"}},
		{routeMetaKey: map[string]any{routeFieldVer: 1, routeFieldTurn: ""}},
		{routeMetaKey: map[string]any{routeFieldVer: 1.5, routeFieldTurn: "turn"}},
		{routeMetaKey: map[string]any{routeFieldVer: 1, routeFieldTurn: strings.Repeat("n", routeTurnNonceMaxBytes+1)}},
		{routeMetaKey: map[string]any{routeFieldVer: 1, routeFieldTurn: "turn", "extra": true}},
	} {
		_, routeErr := parseInboundTurnRoute(meta)
		requireExactUnsupportedField(t, routeErr, routeMetaKey)
	}

	meta, err := stampRouteMeta(map[string]any{"claude": map[string]any{"native": true}}, elicitationScope{
		SessionID: "session-1", TurnNonce: "turn-1", ToolCallID: "tool-1",
	})
	require.NoError(t, err)
	require.Contains(t, meta, "claude")
	require.Equal(t, map[string]any{
		routeFieldVer: 1, routeFieldID: acp.SessionId("session-1"), routeFieldTurn: "turn-1", "toolCallId": acp.ToolCallId("tool-1"),
	}, meta[routeMetaKey])

	_, err = stampRouteMeta(map[string]any{routeMetaKey: map[string]any{}}, elicitationScope{SessionID: "s", TurnNonce: "t"})
	require.ErrorContains(t, err, "collision")
	requestID := "request-1"
	_, err = stampRouteMeta(nil, elicitationScope{SessionID: "s", TurnNonce: "t", ToolCallID: "tool", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")
	_, err = stampRouteMeta(nil, elicitationScope{})
	require.Error(t, err)

	previous := routeRandRead
	routeRandRead = func([]byte) (int, error) { return 0, errors.New("entropy") }
	t.Cleanup(func() { routeRandRead = previous })
	_, err = stampRouteMeta(nil, elicitationScope{SessionID: "s", TurnNonce: "t"})
	require.ErrorContains(t, err, "entropy")
}

func TestRouteCapabilityScalar(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)

	route, ok := resp.AgentCapabilities.Meta["acp-go.dev/route"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, route["version"])
	require.Len(t, route, 1)
}

func TestPromptAndActiveCancelRequireCurrentRoute(t *testing.T) {
	turnCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session := &agentSession{id: "session-1", cancel: cancel, turnNonce: "active-turn"}
	agent := NewAgent()
	agent.sessions[session.id] = session

	_, err := agent.Prompt(turnCtx, acp.PromptRequest{SessionId: session.id})
	require.Error(t, err)
	_, err = session.Prompt(turnCtx, acp.PromptRequest{SessionId: session.id})
	require.Error(t, err)
	require.Error(t, agent.Cancel(turnCtx, acp.CancelNotification{SessionId: session.id}))
	require.Error(t, agent.Cancel(turnCtx, CancelRequest(session.id, "stale-turn")))
}
