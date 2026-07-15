package claudeacp

import (
	"context"
	"errors"
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

func TestRouteEnvelopeHardCutover(t *testing.T) {
	route, err := parseInboundTurnRoute(turnRouteMeta("turn-1"))
	require.NoError(t, err)
	require.Equal(t, "turn-1", route.turnNonce)
	decoded, err := parseInboundTurnRoute(map[string]any{routeMetaKey: map[string]any{
		routeFieldVer: float64(1), routeFieldTurn: "decoded-turn",
	}})
	require.NoError(t, err)
	require.Equal(t, "decoded-turn", decoded.turnNonce)
	require.False(t, routeVersionIsOne("1"))

	for _, meta := range []map[string]any{
		nil,
		{routeMetaKey: "bad"},
		{routeMetaKey: map[string]any{routeFieldVer: 2, routeFieldTurn: "turn"}},
		{routeMetaKey: map[string]any{routeFieldVer: 1, routeFieldTurn: ""}},
		{routeMetaKey: map[string]any{routeFieldVer: 1.5, routeFieldTurn: "turn"}},
		{routeMetaKey: map[string]any{routeFieldVer: 1, routeFieldTurn: "turn", "extra": true}},
	} {
		_, routeErr := parseInboundTurnRoute(meta)
		require.Error(t, routeErr)
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

func TestInitializeAdvertisesRouteV1(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"versions": []int{1}}, resp.AgentCapabilities.Meta[routeMetaKey])
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
