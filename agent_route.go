package claudeacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

const (
	routeMetaKey = "acp-go.dev/route"
	// routeMetaPath is the request path a route refusal names. A refusal names
	// the `_meta` member the host actually wrote, so the bare key alone is never
	// the answer.
	routeMetaPath          = `_meta["` + routeMetaKey + `"]`
	routeVersion           = 1
	routeTurnNonceMaxBytes = 4 * 1024
	routeFieldVer          = "version"
	routeFieldID           = "sessionId"
	routeFieldTurn         = "turnNonce"
)

type inboundTurnRoute struct{ turnNonce string }

type turnRouteContextKey struct{}

type controlCallbackAdmissionContextKey struct{}

// controlCallbackAdmission is the exact ownership reservation made when the
// native controller presents a captured callback route. Autonomous reservations
// name their pump incarnation; prompt-owned callbacks carry nil there because
// their foreground turn is already the causal owner.
type controlCallbackAdmission struct {
	session     *agentSession
	route       string
	incarnation *nativeIncarnation
	autonomous  bool
	done        chan struct{}
}

var routeRandRead = rand.Read

// parseInboundTurnRoute reads the reserved route envelope a prompt or an active
// cancel carries. The two refusals are distinct facts and are never collapsed: a
// key the host omitted is `missing` on the bare path, and a value that is present
// and unacceptable is `unsupported` on the offending member — `version`,
// `turnNonce`, or the unknown key — with the bare path only when the value as a
// whole is not an object. Unknown members are read in sorted order so one request
// always names the same member back.
func parseInboundTurnRoute(meta map[string]any) (inboundTurnRoute, error) {
	raw, present := meta[routeMetaKey]
	if !present {
		return inboundTurnRoute{}, missingField(routeMetaPath)
	}

	object, ok := raw.(map[string]any)
	if !ok {
		return inboundTurnRoute{}, unsupportedField(routeMetaPath)
	}

	for _, key := range slices.Sorted(maps.Keys(object)) {
		if key != routeFieldVer && key != routeFieldTurn {
			return inboundTurnRoute{}, unsupportedField(routeMemberPath(key))
		}
	}

	if !routeVersionIsOne(object[routeFieldVer]) {
		return inboundTurnRoute{}, unsupportedField(routeMemberPath(routeFieldVer))
	}

	nonce, ok := object[routeFieldTurn].(string)
	if !ok || strings.TrimSpace(nonce) == "" || len(nonce) > routeTurnNonceMaxBytes {
		return inboundTurnRoute{}, unsupportedField(routeMemberPath(routeFieldTurn))
	}

	return inboundTurnRoute{turnNonce: nonce}, nil
}

func routeMemberPath(member string) string {
	return routeMetaPath + "." + member
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

func controlCallbackAdmissionFromContext(ctx context.Context) *controlCallbackAdmission {
	if ctx == nil {
		return nil
	}

	admission, _ := ctx.Value(controlCallbackAdmissionContextKey{}).(*controlCallbackAdmission)

	return admission
}

// admitControlCallback linearizes the route the native client captured against
// prompt dispatch. A callback that wins installs an exact ownership reservation
// before its handler continues. A prompt that wins changes the stream owner and
// rotates the autonomous route before releasing the same primitive, so the old
// capture is refused here and can never become live again after prompt handoff.
func (s *agentSession) admitControlCallback(
	ctx context.Context,
	nonce string,
) (context.Context, func(), bool) {
	stream := s.lifecycleStream()

	if !s.awaitEstablishmentRoute(ctx, nonce) {
		return ctx, func() {}, false
	}

	s.callbackOwnershipMu.Lock()

	expected, admitted := s.callbackOwner(stream, nonce)

	if admitted && expected.incarnation != nil &&
		(expected.incarnation.failed.Load() || !s.nativePumpHandle().serves(expected.incarnation)) {
		admitted = false
	}

	if !admitted {
		s.callbackOwnershipMu.Unlock()

		return ctx, func() {}, false
	}

	permitCtx, releasePermit, permitErr := s.agent.acquireCallbackClientCall(ctx)
	if permitErr != nil {
		s.callbackOwnershipMu.Unlock()

		return ctx, func() {}, false
	}

	ctx = permitCtx

	admission := &controlCallbackAdmission{
		session:     s,
		route:       nonce,
		incarnation: expected.incarnation,
		autonomous:  expected.autonomous,
		done:        make(chan struct{}),
	}
	if s.callbackAdmissions == nil {
		s.callbackAdmissions = make(map[*controlCallbackAdmission]struct{})
	}

	s.callbackAdmissions[admission] = struct{}{}
	s.callbackOwnershipMu.Unlock()

	ctx = withTurnRoute(ctx, nonce)
	ctx = context.WithValue(ctx, controlCallbackAdmissionContextKey{}, admission)

	var once sync.Once

	finish := func() {
		once.Do(func() {
			s.callbackOwnershipMu.Lock()
			delete(s.callbackAdmissions, admission)
			s.callbackOwnershipMu.Unlock()
			close(admission.done)
			releasePermit()
		})
	}

	return ctx, finish, true
}

// callbackOwner resolves a captured route while callbackOwnershipMu excludes a
// prompt dispatch. The lifecycle stream is authoritative when negotiated; a
// connection without lifecycle envelopes uses the published prompt route and
// the exact autonomous incarnation.
func (s *agentSession) callbackOwner(
	stream *sessionStream,
	nonce string,
) (controlCallbackOwner, bool) {
	if nonce == "" {
		return controlCallbackOwner{}, false
	}

	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()

	if closing {
		return controlCallbackOwner{}, false
	}

	if stream != nil {
		return stream.callbackOwner(nonce)
	}

	s.mu.Lock()
	promptOwner := s.cancel != nil && s.turnNonce == nonce
	s.mu.Unlock()

	if promptOwner {
		return controlCallbackOwner{incarnation: s.currentNativeIncarnation()}, true
	}

	incarnation := s.autonomousOwner(nonce)
	if incarnation == nil || incarnation.failed.Load() {
		return controlCallbackOwner{}, false
	}

	return controlCallbackOwner{incarnation: incarnation, autonomous: true}, true
}

func (s *agentSession) hasControlCallbackLocked() bool {
	return len(s.callbackAdmissions) != 0
}

// activeControlCallbackContext accepts only the exact admission installed by the
// native controller. A route value alone is never authority: tests and production
// enter through admitControlCallback and carry its registered reservation here.
func (s *agentSession) activeControlCallbackContext(ctx context.Context) (context.Context, bool) {
	if ctx == nil || ctx.Err() != nil {
		return ctx, false
	}

	admission := controlCallbackAdmissionFromContext(ctx)
	if admission == nil || admission.session != s || admission.route == "" {
		return ctx, false
	}

	s.callbackOwnershipMu.Lock()
	defer s.callbackOwnershipMu.Unlock()

	if _, live := s.callbackAdmissions[admission]; !live {
		return ctx, false
	}

	owner, live := s.callbackOwner(s.lifecycleStream(), admission.route)
	if !live || owner.incarnation != admission.incarnation || owner.autonomous != admission.autonomous {
		return ctx, false
	}

	return ctx, true
}

func turnRouteMetaFromContext(ctx context.Context) map[string]any {
	turnNonce := turnNonceFromContext(ctx)
	if turnNonce == "" {
		return nil
	}

	return turnRouteMeta(turnNonce)
}
