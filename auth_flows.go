package claudeacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
)

// Closed flow states.
const (
	authStatePending       = "pending"
	authStateAuthenticated = "authenticated"
	authStateFailed        = "failed"
	authStateCancelled     = "cancelled"
	authStateExpired       = "expired"
)

// Closed flow reasons, legal only against the state each pairs with.
const (
	authReasonProviderRefused   = "provider_refused"
	authReasonNativeVeto        = "native_veto"
	authReasonTransport         = "transport"
	authReasonProcess           = "process"
	authReasonAcceptanceUnknown = "acceptance_unknown"
	authReasonHarvestFailed     = "harvest_failed"
	authReasonOwnerCancel       = "owner_cancel"
	authReasonSuperseded        = "superseded"
	authReasonSessionClosed     = "session_closed"
	authReasonDeadline          = "deadline"
)

// Closed interaction discriminator. Claude's single method is a hosted
// paste-back login, so the only value this adapter ever emits is "callback".
const authInteractionCallback = "callback"

const authCallbackInputCode = "code"

// authPasteSeparator joins the two halves of the pasted value. The harness
// expects `<code>#<state>`: a leg relaying only the code half cannot complete
// this flow.
const authPasteSeparator = "#"

const (
	// authSafetyDeadline bounds a flow independently of the harness, which
	// supplies no expiry of its own on this surface.
	authSafetyDeadline = 15 * time.Minute
	// authPollFloor is the fastest cadence a status call may drive a native
	// read at, so consumer poll cadence never propagates into a provider.
	authPollFloor = 5 * time.Second
	// authMaxTextInputBytes bounds one submitted value.
	authMaxTextInputBytes = 1024
	// authChildExitWait bounds how long callback waits for the login child to
	// settle after the pasted value crosses.
	authChildExitWait = 2 * time.Minute
)

var (
	authRandRead = rand.Read
	authNow      = time.Now
)

// authFlow is the session-scoped record of one login. The presentation it can
// replay lives here and nowhere else: it carries url and message, which are
// code-bearing for the flow's life.
type authFlow struct {
	id                 string
	sessionID          acp.SessionId
	providerID         string
	connectionID       string
	revision           int64
	bindingGeneration  int64
	method             authCatalogMethod
	authorizeRequestID string
	presentation       authAuthorizeResult

	createdAt int64
	state     string
	reason    string
	expiresAt time.Time

	login *authLoginHandle

	nextProbeAt time.Time

	disarm chan struct{}
}

type authAuthorizeResult struct {
	Interaction   string `json:"interaction"`
	URL           string `json:"url,omitempty"`
	Message       string `json:"message"`
	CallbackInput string `json:"callbackInput,omitempty"`
	FlowID        string `json:"flowId"`
	FlowExpiresAt int64  `json:"flowExpiresAt"`
}

type authFlowIDResult struct {
	FlowID string `json:"flowId"`
}

type authStatusResult struct {
	FlowID string `json:"flowId"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

func authTerminal(state string) bool {
	return state != authStatePending
}

// newAuthToken mints an opaque adapter-owned identifier from 16 CSPRNG bytes,
// encoded unpadded base64url. Native flow handles never cross the boundary.
func newAuthToken() (string, error) {
	var value [16]byte
	if _, err := authRandRead(value[:]); err != nil {
		return "", fmt.Errorf("create provider auth token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

type authorizeRequest struct {
	sessionID    string
	providerID   string
	connectionID string
	generation   string
	method       string
	// authorizeRequestID is the caller-minted idempotency key. authorize is the
	// only leg that takes one because it is the most destructive leg here.
	authorizeRequestID string
	inputs             map[string]string
}

func decodeAuthorizeRequest(fields map[string]json.RawMessage) (authorizeRequest, error) {
	request := authorizeRequest{}

	var err error
	if request.sessionID, err = authRequiredString(fields, authFieldSessionID); err != nil {
		return request, err
	}

	if request.providerID, err = authRequiredString(fields, authFieldProviderID); err != nil {
		return request, err
	}

	if request.connectionID, err = authRequiredString(fields, authFieldConnectionID); err != nil {
		return request, err
	}

	if request.generation, err = authRequiredString(fields, authFieldMethodsGeneration); err != nil {
		return request, err
	}

	if request.method, err = authRequiredString(fields, authFieldMethod); err != nil {
		return request, err
	}

	if request.authorizeRequestID, err = authRequiredString(fields, authFieldAuthorizeRequestID); err != nil {
		return request, err
	}

	if raw, ok := fields[authFieldInputs]; ok {
		if err := json.Unmarshal(raw, &request.inputs); err != nil {
			return request, invalidAuthField(authFieldInputs)
		}
	}

	return request, nil
}

// authorize starts exactly one flow per (sessionId, providerId). It records the
// idempotency key before any native mint and has persisted the flow's slot
// binding before it returns.
func (p *providerAuth) authorize(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params,
		authFieldSessionID, authFieldProviderID, authFieldConnectionID,
		authFieldMethodsGeneration, authFieldMethod, authFieldAuthorizeRequestID, authFieldInputs)
	if err != nil {
		return nil, err
	}

	request, err := decodeAuthorizeRequest(fields)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(request.sessionID)
	if err != nil {
		return nil, err
	}

	key := authFlowKey{sessionID: session.id, providerID: request.providerID}

	if replay, ok := p.replayAuthorize(key, request.authorizeRequestID); ok {
		return replay, nil
	}

	method, err := p.resolveMethod(request)
	if err != nil {
		return nil, err
	}

	if inputErr := validateAuthInputs(request.inputs); inputErr != nil {
		return nil, inputErr
	}

	flowID, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	p.supersede(key, authReasonSuperseded)

	record, err := p.recordAuthorizeIntent(request, flowID)
	if err != nil {
		return nil, err
	}

	now := authNow()
	flow := &authFlow{
		id:                 flowID,
		sessionID:          session.id,
		providerID:         request.providerID,
		connectionID:       request.connectionID,
		revision:           record.Revision,
		bindingGeneration:  record.BindingGeneration,
		method:             method,
		authorizeRequestID: request.authorizeRequestID,
		createdAt:          record.CreatedAt,
		state:              authStatePending,
		expiresAt:          now.Add(authSafetyDeadline),
		disarm:             make(chan struct{}),
	}

	presentation, err := p.mintPresentation(ctx, flow)
	if err != nil {
		return nil, err
	}

	flow.presentation = presentation

	p.mu.Lock()
	p.flows[key] = flow
	p.byID[flowID] = flow
	p.mu.Unlock()

	p.armCompleter(flow)

	return presentation, nil
}

// mintPresentation starts the login child and builds the wire presentation. The
// authorization URL is validated by the grammar independently and before any
// line matching, then bounded again here before it is relayed.
func (p *providerAuth) mintPresentation(ctx context.Context, flow *authFlow) (authAuthorizeResult, error) {
	login, authorizeURL, err := p.startLogin(ctx)
	if err != nil {
		return authAuthorizeResult{}, err
	}

	bounded, ok := authDisplayURL(authorizeURL)
	if !ok {
		login.close()

		return authAuthorizeResult{}, authFailed(authCauseNativeVeto, flow.providerID, flow.method.ID, flow.id)
	}

	message, ok := authDisplayText(flow.method.Label, authMaxMessageBytes)
	if !ok {
		login.close()

		return authAuthorizeResult{}, authFailed(authCauseNativeVeto, flow.providerID, flow.method.ID, flow.id)
	}

	flow.login = login

	return authAuthorizeResult{
		Interaction:   flow.method.Interaction,
		URL:           bounded,
		Message:       message,
		CallbackInput: authCallbackInputCode,
		FlowID:        flow.id,
		FlowExpiresAt: flow.expiresAt.UnixMilli(),
	}, nil
}

// replayAuthorize answers a repeated idempotency key verbatim from memory: no
// supersede, no completer disarm, no destruction of flow state, and no native
// call.
func (p *providerAuth) replayAuthorize(key authFlowKey, requestID string) (authAuthorizeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.flows[key]
	if !ok || flow.authorizeRequestID != requestID {
		return authAuthorizeResult{}, false
	}

	return flow.presentation, true
}

// resolveMethod fences a method id against the generation that produced it.
func (p *providerAuth) resolveMethod(request authorizeRequest) (authCatalogMethod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.generation == "" || p.generation != request.generation {
		return authCatalogMethod{}, invalidAuthField(authFieldMethodsGeneration)
	}

	for _, method := range p.catalog[request.providerID] {
		if method.ID == request.method {
			return method, nil
		}
	}

	return authCatalogMethod{}, invalidAuthField(authFieldMethod)
}

// armCompleter bounds the flow by its effective deadline. It is armed exactly
// once, at authorize, and status never starts, extends, or rearms it.
func (p *providerAuth) armCompleter(flow *authFlow) {
	deadline := time.Until(flow.expiresAt)
	disarm := flow.disarm

	p.goSafe(func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()

		select {
		case <-disarm:
			return
		case <-timer.C:
			p.expire(flow)
		}
	})
}

func (p *providerAuth) expire(flow *authFlow) {
	p.mu.Lock()

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return
	}

	flow.state = authStateExpired
	flow.reason = authReasonDeadline
	login := flow.takeLogin()

	delete(p.flows, authFlowKey{sessionID: flow.sessionID, providerID: flow.providerID})
	p.mu.Unlock()

	login.close()
}

// supersede terminalizes the flow a new authorize replaces. Its login child is
// terminated first, so a stale approval cannot land against a flow that no
// longer addresses anything.
func (p *providerAuth) supersede(key authFlowKey, reason string) {
	p.mu.Lock()

	flow, ok := p.flows[key]
	if !ok {
		p.mu.Unlock()

		return
	}

	delete(p.flows, key)

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return
	}

	flow.state = authStateCancelled
	flow.reason = reason
	login := flow.takeLogin()

	flow.stopCompleter()
	p.mu.Unlock()

	login.close()
}

func (f *authFlow) stopCompleter() {
	select {
	case <-f.disarm:
	default:
		close(f.disarm)
	}
}

func (f *authFlow) takeLogin() *authLoginHandle {
	login := f.login
	f.login = nil

	return login
}

// callback submits the flow's expected value. The submitted input is
// credential-class: it is written to the login child and never persisted,
// logged, or echoed.
func (p *providerAuth) callback(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldMethod, authFieldFlowID, authFieldInput)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	method, err := authRequiredString(fields, authFieldMethod)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	input, err := authString(fields, authFieldInput)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	flow, err := p.addressFlow(session.id, providerID, flowID)
	if err != nil {
		return nil, err
	}

	if flow.method.ID != method {
		return nil, invalidAuthField(authFieldMethod)
	}

	if err := validateAuthPastedValue(input); err != nil {
		return nil, err
	}

	return p.submitPastedValue(ctx, flow, input)
}

// validateAuthPastedValue bounds the pasted value and pins its shape. The
// harness expects `<code>#<state>`; a bare code is refused here rather than
// silently failing the login.
func validateAuthPastedValue(input string) error {
	if input == "" || len(input) > authMaxTextInputBytes || !utf8.ValidString(input) {
		return invalidAuthField(authFieldInput)
	}

	for _, char := range input {
		if char == '\n' || char == '\r' || unicode.IsControl(char) {
			return invalidAuthField(authFieldInput)
		}
	}

	code, state, ok := strings.Cut(input, authPasteSeparator)
	if !ok || code == "" || state == "" || strings.Contains(state, authPasteSeparator) {
		return invalidAuthField(authFieldInput)
	}

	return nil
}

// submitPastedValue writes the value to the login child, waits for it to
// settle, and then reads the one completion signal this surface has: the
// `auth status --json` exit code. The login process's stdout never signals
// success.
func (p *providerAuth) submitPastedValue(ctx context.Context, flow *authFlow, input string) (any, error) {
	p.mu.Lock()

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return nil, authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	login := flow.login
	p.mu.Unlock()

	if login == nil {
		return nil, p.fail(flow, authCauseProcess, false)
	}

	if err := login.submit(input); err != nil {
		return nil, p.fail(flow, authCauseProcess, true)
	}

	waitCtx, cancel := context.WithTimeout(ctx, authChildExitWait)
	defer cancel()

	p.awaitLoginExit(waitCtx, login)

	if err := p.settle(ctx, flow); err != nil {
		return nil, err
	}

	return authFlowIDResult{FlowID: flow.id}, nil
}

// awaitLoginExit closes the login child and waits for its containment boundary,
// bounded by the caller's deadline. The child exits by itself once the harness
// finishes the exchange; the close is the fence for every other outcome.
func (p *providerAuth) awaitLoginExit(ctx context.Context, login *authLoginHandle) {
	done := make(chan struct{})

	p.goSafe(func() {
		login.close()
		close(done)
	})

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// settle reads the completion signal and terminalizes the flow. A logged-in
// config dir is authenticated; anything else is a provider refusal.
func (p *providerAuth) settle(ctx context.Context, flow *authFlow) error {
	_, loggedIn, err := p.probeAccount(ctx)
	if err != nil {
		return p.fail(flow, authCauseTransport, true)
	}

	if !loggedIn {
		return p.fail(flow, authCauseProviderRefused, true)
	}

	if err := p.confirmAuthorize(flow); err != nil {
		return err
	}

	p.terminalize(flow, authStateAuthenticated, "")

	return nil
}

// fail returns the leg's closed error and performs the transition its cause
// pairs with. A cause with no transition consumes nothing.
func (p *providerAuth) fail(flow *authFlow, cause string, materialInFlight bool) error {
	if state, reason := authFlowTransition(cause, materialInFlight); state != "" {
		p.terminalize(flow, state, reason)
	}

	return authFailed(cause, flow.providerID, flow.method.ID, flow.id)
}

func (p *providerAuth) terminalize(flow *authFlow, state string, reason string) {
	p.mu.Lock()

	flow.state = state
	flow.reason = reason
	login := flow.takeLogin()

	flow.stopCompleter()
	delete(p.flows, authFlowKey{sessionID: flow.sessionID, providerID: flow.providerID})
	p.mu.Unlock()

	login.close()
}

// addressFlow resolves a flowId a caller supplied. A missing, unknown,
// superseded, or cross-session id is a caller addressing failure and never a
// flow failure.
func (p *providerAuth) addressFlow(sessionID acp.SessionId, providerID string, flowID string) (*authFlow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.byID[flowID]
	if !ok || flow.sessionID != sessionID || flow.providerID != providerID {
		return nil, invalidAuthField(authFieldFlowID)
	}

	return flow, nil
}

// status reports the flow, not the connection. Claude's credential carries no
// expiry this surface may read, so expiresAt is never emitted; it is credential
// expiry and would never be flow expiry.
func (p *providerAuth) status(ctx context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.probe(ctx, flow)

	p.mu.Lock()
	defer p.mu.Unlock()

	return authStatusResult{FlowID: flow.id, State: flow.state, Reason: flow.reason}, nil
}

// probe settles a pending flow whose login child has already exited, which is
// the only signal that a login completed without a callback: the harness arms a
// loopback hook unconditionally, so a URL opened on this host self-completes.
// The native read runs no faster than the poll floor, and the cached state is
// served in between.
func (p *providerAuth) probe(ctx context.Context, flow *authFlow) {
	p.mu.Lock()

	now := authNow()
	if authTerminal(flow.state) || flow.login == nil || now.Before(flow.nextProbeAt) {
		p.mu.Unlock()

		return
	}

	flow.nextProbeAt = now.Add(authPollFloor)
	login := flow.login
	p.mu.Unlock()

	if !login.exited() {
		return
	}

	_, loggedIn, err := p.probeAccount(ctx)
	if err != nil || !loggedIn {
		return
	}

	if err := p.confirmAuthorize(flow); err != nil {
		return
	}

	p.terminalize(flow, authStateAuthenticated, "")
}

// cancel is adapter-owned: claude has no native flow cancel, so the leg does
// everything the wrapper owns and its process kill is the fence. It claims
// nothing about the provider — an issued authorization stays valid there until
// it expires.
func (p *providerAuth) cancel(_ context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return authFlowIDResult{FlowID: flow.id}, nil
	}

	flow.state = authStateCancelled
	flow.reason = authReasonOwnerCancel
	login := flow.takeLogin()

	flow.stopCompleter()
	delete(p.flows, authFlowKey{sessionID: flow.sessionID, providerID: flow.providerID})
	p.mu.Unlock()

	login.close()

	return authFlowIDResult{FlowID: flow.id}, nil
}

func (p *providerAuth) addressedFlowLeg(params json.RawMessage) (*authFlow, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	return p.addressFlow(session.id, providerID, flowID)
}

// closeSession cancels every pending flow the session owns, terminalizing each
// as cancelled/session_closed and killing its login child. It runs after
// pending elicitation is resolved and before the native interrupt, so a flow is
// never abandoned to a process already being torn down.
func (p *providerAuth) closeSession(sessionID acp.SessionId) {
	if p == nil {
		return
	}

	p.mu.Lock()

	logins := make([]*authLoginHandle, 0, len(p.flows))

	for key, flow := range p.flows {
		if key.sessionID != sessionID {
			continue
		}

		delete(p.flows, key)

		if authTerminal(flow.state) {
			continue
		}

		flow.state = authStateCancelled
		flow.reason = authReasonSessionClosed

		flow.stopCompleter()
		logins = append(logins, flow.takeLogin())
	}

	p.mu.Unlock()

	for _, login := range logins {
		login.close()
	}
}
