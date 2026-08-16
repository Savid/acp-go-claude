package claudeacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	routeMetaKey           = "acp-go.dev/route"
	routeVersion           = 1
	routeTurnNonceMaxBytes = 4 * 1024
	routeFieldVer          = "version"
	routeFieldID           = "sessionId"
	routeFieldTurn         = "turnNonce"
)

type inboundTurnRoute struct{ turnNonce string }

type turnRouteContextKey struct{}

var routeRandRead = rand.Read

func parseInboundTurnRoute(meta map[string]any) (inboundTurnRoute, error) {
	object, ok := meta[routeMetaKey].(map[string]any)
	if !ok || len(object) != 2 || !routeVersionIsOne(object[routeFieldVer]) {
		return inboundTurnRoute{}, unsupportedField(routeMetaKey)
	}

	nonce, ok := object[routeFieldTurn].(string)
	if !ok || strings.TrimSpace(nonce) == "" || len(nonce) > routeTurnNonceMaxBytes {
		return inboundTurnRoute{}, unsupportedField(routeMetaKey)
	}

	return inboundTurnRoute{turnNonce: nonce}, nil
}

func routeVersionIsOne(value any) bool {
	switch version := value.(type) {
	case int:
		return version == routeVersion
	case float64:
		return version == routeVersion
	default:
		return false
	}
}

func stampRouteMeta(meta map[string]any, scope elicitationScope) (map[string]any, error) {
	if scope.SessionID == "" || strings.TrimSpace(scope.TurnNonce) == "" {
		return nil, fmt.Errorf("route metadata requires sessionId and turnNonce")
	}

	if _, exists := meta[routeMetaKey]; exists {
		return nil, fmt.Errorf("reserved route metadata collision")
	}

	route := map[string]any{routeFieldVer: routeVersion, routeFieldID: scope.SessionID, routeFieldTurn: scope.TurnNonce}

	correlations := 0
	if scope.ToolCallID != "" {
		correlations++
		route["toolCallId"] = scope.ToolCallID
	}

	if scope.RequestID != nil && *scope.RequestID != "" {
		correlations++
		route["requestId"] = *scope.RequestID
	}

	if correlations == 0 {
		requestID, err := newRouteRequestID()
		if err != nil {
			return nil, err
		}

		correlations++
		route["requestId"] = requestID
	}

	if correlations != 1 {
		return nil, fmt.Errorf("route metadata requires exactly one callback correlation")
	}

	out := cloneAnyMap(meta)
	if out == nil {
		out = map[string]any{}
	}

	out[routeMetaKey] = route

	return out, nil
}

func newRouteRequestID() (string, error) {
	var data [16]byte
	if _, err := routeRandRead(data[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(data[:]), nil
}

func turnRouteMeta(turnNonce string) map[string]any {
	return map[string]any{routeMetaKey: map[string]any{routeFieldVer: routeVersion, routeFieldTurn: turnNonce}}
}

// requestTurnRouteMeta validates untrusted caller input used by the exported
// request builders. Internal turn scopes have already passed inbound route
// validation and continue to use turnRouteMeta directly.
func requestTurnRouteMeta(turnNonce string) map[string]any {
	if strings.TrimSpace(turnNonce) == "" || len(turnNonce) > routeTurnNonceMaxBytes {
		return nil
	}

	return turnRouteMeta(turnNonce)
}

func withTurnRoute(ctx context.Context, turnNonce string) context.Context {
	return context.WithValue(ctx, turnRouteContextKey{}, turnNonce)
}

func turnNonceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	turnNonce, _ := ctx.Value(turnRouteContextKey{}).(string)

	return turnNonce
}

// activeControlCallbackContext accepts only a callback admitted for the exact
// active prompt turn. The callback context owns its captured nonce; this never
// consults the session nonce as a fallback and therefore cannot rebind an old
// callback to a newer turn.
func (s *agentSession) activeControlCallbackContext(ctx context.Context) (context.Context, bool) {
	if ctx == nil || ctx.Err() != nil {
		return ctx, false
	}

	callbackNonce := turnNonceFromContext(ctx)

	s.mu.Lock()
	activeNonce := s.turnNonce
	active := s.cancel != nil && activeNonce != ""
	s.mu.Unlock()

	if !active || callbackNonce == "" || callbackNonce != activeNonce {
		return ctx, false
	}

	return withTurnRoute(ctx, activeNonce), true
}

func turnRouteMetaFromContext(ctx context.Context) map[string]any {
	turnNonce := turnNonceFromContext(ctx)
	if turnNonce == "" {
		return nil
	}

	return turnRouteMeta(turnNonce)
}
