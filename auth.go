package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// Session-scoped provider-auth extension methods. Claude brokers no credential
// back out — a completed login installs into the config dir the harness already
// owns — so there is no credential leg and no injection key.
const (
	AuthMethodsMethod    = "_claude/auth/methods"
	AuthAuthorizeMethod  = "_claude/auth/authorize"
	AuthCallbackMethod   = "_claude/auth/callback"
	AuthStatusMethod     = "_claude/auth/status"
	AuthCancelMethod     = "_claude/auth/cancel"
	AuthInventoryMethod  = "_claude/auth/inventory"
	AuthDisconnectMethod = "_claude/auth/disconnect"
)

const (
	providerAuthCapabilityKey = "providerAuth"
	providerAuthMethodsField  = "methods"

	authFailedErrorTag = "claude_auth_failed"

	authFieldParams             = "params"
	authFieldSessionID          = acpFieldSessionID
	authFieldProviderID         = "providerId"
	authFieldConnectionID       = "connectionId"
	authFieldMethodsGeneration  = "methodsGeneration"
	authFieldMethod             = jsonFieldMethod
	authFieldAuthorizeRequestID = "authorizeRequestId"
	authFieldInputs             = "inputs"
	authFieldFlowID             = "flowId"
	authFieldInput              = "input"
	authFieldBindingGeneration  = "bindingGeneration"
	authFieldRetryable          = "retryable"

	authErrValueInvalid = "invalid"
)

// Closed cause enum returned by a provider-auth leg. No native message, no
// native response body, and no free-form field ever joins it.
const (
	authCauseNativeVeto         = "native_veto"
	authCauseProviderRefused    = "provider_refused"
	authCauseTransport          = "transport"
	authCauseProcess            = "process"
	authCauseTimeout            = "timeout"
	authCauseHarvestFailed      = "harvest_failed"
	authCauseUnsupportedVariant = "unsupported_variant"
	authCauseFlowExpired        = "flow_expired"
	authCauseFlowState          = "flow_state"
	authCausePolicy             = "policy"
)

// Native seams. Every provider-auth native read and mutation crosses one of
// these, so the whole surface is drivable without a Claude CLI.
var (
	authStatusProbe    = claude.AuthStatus
	authLogoutCommand  = claude.AuthLogout
	authLoginStart     = claude.StartAuthLogin
	authKeychainRemove = claude.RemoveAuthKeychainItems
)

// providerAuth is the agent-scoped broker behind the provider-auth legs. It
// owns the pinned catalog, the per-session flow records, and the durable
// values-free ledger.
type providerAuth struct {
	agent  *Agent
	ledger *authLedger
	// directHome reports whether the operator named this exact native home as
	// one a native account-level removal may clear. Without it the disconnect
	// leg is absent from the advertisement and returns method-not-found.
	directHome bool

	mu         sync.Mutex
	generation string
	catalog    map[string][]authCatalogMethod
	flows      map[authFlowKey]*authFlow
	byID       map[string]*authFlow
}

type authFlowKey struct {
	sessionID  acp.SessionId
	providerID string
}

// newProviderAuth builds the broker when a usable durable ledger root is
// configured. A root that is absent, unusable, or unwritable leaves the surface
// unadvertised on exactly the terms an unset one does: a leg that cannot record
// what it did must not be offered.
func newProviderAuth(agent *Agent) *providerAuth {
	if !authLedgerRootConfigured(agent.options) {
		return nil
	}

	ledger, err := newAuthLedger(agent.options)
	if err != nil {
		agent.log.Warn("provider auth surface is unavailable", slog.String(jsonFieldError, err.Error()))

		return nil
	}

	return &providerAuth{
		agent:      agent,
		ledger:     ledger,
		directHome: providerAuthDirectHome(agent.options),
		flows:      make(map[authFlowKey]*authFlow),
		byID:       make(map[string]*authFlow),
	}
}

// providerAuthDirectHome reports whether the exact-home consent gate authorizes
// this agent's native home. Both sides are resolved the way the leg itself
// resolves the home it acts on, so consent covers exactly the directory a
// removal clears rather than a name that happens to point at it.
func providerAuthDirectHome(options Options) bool {
	if strings.TrimSpace(options.ProviderAuthDirectHome) == "" || strings.TrimSpace(options.Home) == "" {
		return false
	}

	direct, err := canonicalClaudeHome(options.ProviderAuthDirectHome)
	if err != nil {
		return false
	}

	home, err := canonicalClaudeHome(options.Home)
	if err != nil {
		return false
	}

	return direct == home
}

// validateProviderAuthDirectHome fails an agent whose consent gate is relative,
// joining the same construction verdict a relative handoff root and a negative
// image limit reach.
func validateProviderAuthDirectHome(directHome string) error {
	if directHome == "" || filepath.IsAbs(directHome) {
		return nil
	}

	return errors.New("ProviderAuthDirectHome must be an absolute path")
}

// authMethodNames lists every advertised leg in the order the capability
// reports them. An absent leg is omitted rather than reported false, because
// the array is the host's only discovery surface for which legs exist.
func (p *providerAuth) authMethodNames() []string {
	names := []string{
		AuthMethodsMethod,
		AuthAuthorizeMethod,
		AuthCallbackMethod,
		AuthStatusMethod,
		AuthCancelMethod,
		AuthInventoryMethod,
	}

	if p.directHome {
		names = append(names, AuthDisconnectMethod)
	}

	return names
}

// capability reports the enabled leg names. Claude carries no injection key:
// the option field is unsupported and fails closed with its path like any other
// unknown key.
func (p *providerAuth) capability() map[string]any {
	return map[string]any{providerAuthMethodsField: p.authMethodNames()}
}

func (p *providerAuth) advertises(method string) bool {
	for _, name := range p.authMethodNames() {
		if name == method {
			return true
		}
	}

	return false
}

// handleAuthExtensionMethod dispatches an advertised leg. The second result
// reports whether this surface owns the method at all, so an unadvertised leg
// falls through to the uniform method-not-found.
func (a *Agent) handleAuthExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, bool, error) {
	broker := a.providerAuth
	if broker == nil || !broker.advertises(method) {
		return nil, false, nil
	}

	switch method {
	case AuthMethodsMethod:
		result, err := broker.methods(ctx, params)

		return result, true, err
	case AuthAuthorizeMethod:
		result, err := broker.authorize(ctx, params)

		return result, true, err
	case AuthCallbackMethod:
		result, err := broker.callback(ctx, params)

		return result, true, err
	case AuthStatusMethod:
		result, err := broker.status(ctx, params)

		return result, true, err
	case AuthCancelMethod:
		result, err := broker.cancel(ctx, params)

		return result, true, err
	case AuthInventoryMethod:
		result, err := broker.inventory(ctx, params)

		return result, true, err
	default:
		result, err := broker.disconnect(ctx, params)

		return result, true, err
	}
}

// authFailedError is the uniform provider-auth leg failure. Native message
// text, native response bodies, and captured child output never reach it: a
// native failure payload can carry an entire upstream HTTP response.
type authFailedError struct {
	cause      string
	providerID string
	method     string
	flowID     string
}

func (f *authFailedError) Error() string {
	return authFailedErrorTag + ": " + f.cause
}

func (f *authFailedError) requestError() *acp.RequestError {
	data := map[string]any{
		jsonFieldError:     authFailedErrorTag,
		failureFieldCause:  f.cause,
		authFieldRetryable: authCauseRetryable(f.cause),
	}

	if f.providerID != "" {
		data[authFieldProviderID] = f.providerID
	}

	if f.method != "" {
		data[authFieldMethod] = f.method
	}

	if f.flowID != "" {
		data[authFieldFlowID] = f.flowID
	}

	return acp.NewAuthRequired(data)
}

// authCauseRetryable reports whether the same call could succeed unchanged. The
// three transport-shaped causes can; a refusal, a veto, and every flow-state
// answer cannot, because repeating them changes nothing.
func authCauseRetryable(cause string) bool {
	switch cause {
	case authCauseTransport, authCauseProcess, authCauseTimeout:
		return true
	default:
		return false
	}
}

func authFailed(cause string, providerID string, method string, flowID string) error {
	failure := &authFailedError{cause: cause, providerID: providerID, method: method, flowID: flowID}

	return failure.requestError()
}

// authFlowTransition maps a leg cause to the transition it must also perform.
// An empty state means the cause carries none: a refusal the adapter made
// itself never consumes the owner's authorization.
func authFlowTransition(cause string, materialInFlight bool) (string, string) {
	switch cause {
	case authCauseNativeVeto, authCauseUnsupportedVariant:
		return authStateFailed, authReasonNativeVeto
	case authCauseProviderRefused:
		return authStateFailed, authReasonProviderRefused
	case authCauseTransport:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonTransport
	case authCauseProcess:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonProcess
	case authCauseTimeout:
		if materialInFlight {
			return authStateFailed, authReasonAcceptanceUnknown
		}

		return authStateFailed, authReasonTransport
	case authCauseHarvestFailed:
		return authStateFailed, authReasonHarvestFailed
	case authCauseFlowExpired:
		return authStateExpired, authReasonDeadline
	default:
		return "", ""
	}
}

// authSession resolves the session a leg addresses. An unknown, unloaded, or
// tombstoned session gets the uniform unknown-session rejection.
func (p *providerAuth) authSession(id string) (*agentSession, error) {
	return p.agent.session(acp.SessionId(id))
}

// authParamFields walks a leg's params object once, rejecting an unknown field,
// a duplicate field, and a non-object body with the offending field path. Every
// request object on this surface is closed, and encoding/json alone would let a
// duplicate key silently win.
func authParamFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	permitted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		permitted[name] = struct{}{}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalidAuthField(authFieldParams)
	}

	fields := make(map[string]json.RawMessage, len(allowed))

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, invalidAuthField(authFieldParams)
		}

		key, _ := keyToken.(string)
		if _, ok := permitted[key]; !ok {
			return nil, unsupportedField(key)
		}

		if _, duplicate := fields[key]; duplicate {
			return nil, unsupportedField(key)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalidAuthField(key)
		}

		fields[key] = value
	}

	if _, err := decoder.Token(); err != nil {
		return nil, invalidAuthField(authFieldParams)
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidAuthField(authFieldParams)
	}

	return fields, nil
}

// authRequiredString decodes a non-empty string field.
func authRequiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", invalidAuthField(name)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", invalidAuthField(name)
	}

	return value, nil
}

// authString decodes a string field that may be empty but must be present.
func authString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", invalidAuthField(name)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidAuthField(name)
	}

	return value, nil
}

func authRequiredInt64(fields map[string]json.RawMessage, name string) (int64, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, invalidAuthField(name)
	}

	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, invalidAuthField(name)
	}

	return value, nil
}

// goSafe runs one broker-owned goroutine under the agent's panic recovery.
func (p *providerAuth) goSafe(fn func()) {
	go func() {
		defer recoverAgentGoroutine(context.Background(), p.agent.log, "provider auth")

		fn()
	}()
}

func invalidAuthField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: authErrValueInvalid,
		jsonFieldField: path,
	})
}
