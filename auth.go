package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// Session-scoped provider-auth extension methods.
const (
	AuthMethodsMethod    = "_claude/auth/methods"
	AuthAuthorizeMethod  = "_claude/auth/authorize"
	AuthCallbackMethod   = "_claude/auth/callback"
	AuthStatusMethod     = "_claude/auth/status"
	AuthCancelMethod     = "_claude/auth/cancel"
	AuthInventoryMethod  = "_claude/auth/inventory"
	AuthCredentialMethod = "_claude/auth/credential" //nolint:gosec // Protocol method name, not a credential.
	AuthDisconnectMethod = "_claude/auth/disconnect"
)

const (
	providerAuthCapabilityKey = "providerAuth"
	providerAuthMethodsField  = "methods"
	providerAuthInjectionKey  = "injectionKey"
	providerAuthOptionPath    = "_meta.claude.options.providerAuth"

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
	authCauseFlowCancelled      = "flow_cancelled"
	authCausePolicy             = "policy"
	authCauseBindingConflict    = "binding_conflict"
)

const (
	providerAuthEnvAnthropicAPIKey  = "ANTHROPIC_API_KEY"     //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvAnthropicToken   = "ANTHROPIC_AUTH_TOKEN"  //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvAnthropicAWSKey  = "ANTHROPIC_AWS_API_KEY" //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvAnthropicFoundry = "ANTHROPIC_FOUNDRY_API_KEY"
	providerAuthEnvFoundryToken     = "ANTHROPIC_FOUNDRY_AUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvBedrockToken     = "AWS_BEARER_TOKEN_BEDROCK"     //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvClaudeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"      //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvUseBedrock       = "CLAUDE_CODE_USE_BEDROCK"      //nolint:gosec // Environment variable name, not a credential.
	providerAuthEnvUseFoundry       = "CLAUDE_CODE_USE_FOUNDRY"
	providerAuthEnvUseVertex        = "CLAUDE_CODE_USE_VERTEX"
)

var providerAuthCredentialEnvNames = [...]string{
	providerAuthEnvAnthropicAPIKey,
	providerAuthEnvAnthropicToken,
	providerAuthEnvAnthropicAWSKey,
	providerAuthEnvAnthropicFoundry,
	providerAuthEnvFoundryToken,
	providerAuthEnvBedrockToken,
	providerAuthEnvClaudeOAuthToken,
	providerAuthEnvUseBedrock,
	providerAuthEnvUseFoundry,
	providerAuthEnvUseVertex,
}

const (
	providerAuthUnavailableBareMode = "bare mode"
	providerAuthUnavailableEnv      = "configured credential"
	providerAuthUnavailableSettings = "static settings"
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
	// home is the native config dir every leg on this surface acts on, resolved
	// once here rather than per leg.
	home providerAuthHome
	// directHome reports whether the operator named this exact native home as
	// one a native account-level removal may clear.
	directHome bool

	mu         sync.Mutex
	generation string
	catalog    map[string][]authCatalogMethod
	flows      map[authFlowKey]*authFlow
	byID       map[string]*authFlow
	// closedSessions marks every session whose flows a close already swept. The
	// id survives the session, because the leg that has to be refused is the one
	// still in flight when the sweep ran.
	closedSessions map[acp.SessionId]struct{}
	// retired holds, per flow key, the idempotency keys a later authorize
	// replaced. They are kept for the session's whole life: a retry can be
	// arbitrarily delayed and is still unanswerable when it lands.
	retired    map[authFlowKey]map[string]struct{}
	admissions map[authFlowKey]*authGate
	slots      map[string]*authGate
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

	home := resolveProviderAuthHome(agent.options)
	if reason := providerAuthUnavailableReason(agent.options, home); reason != "" {
		agent.log.Info("interactive provider auth disabled", slog.String("reason", reason))

		return nil
	}

	ledger, err := newAuthLedger(agent.options)
	if err != nil {
		agent.log.Warn("provider auth surface is unavailable", slog.String(jsonFieldError, err.Error()))

		return nil
	}

	return &providerAuth{
		agent:          agent,
		ledger:         ledger,
		home:           home,
		directHome:     providerAuthDirectHome(agent.options, home),
		flows:          make(map[authFlowKey]*authFlow),
		byID:           make(map[string]*authFlow),
		closedSessions: make(map[acp.SessionId]struct{}),
		retired:        make(map[authFlowKey]map[string]struct{}),
		admissions:     make(map[authFlowKey]*authGate),
		slots:          make(map[string]*authGate),
	}
}

func providerAuthUnavailableReason(options Options, home providerAuthHome) string {
	if options.BareMode {
		return providerAuthUnavailableBareMode
	}

	if providerAuthCredentialEnvironmentConfigured(options) {
		return providerAuthUnavailableEnv
	}

	if providerAuthStaticSettingsConfigured(options, home) {
		return providerAuthUnavailableSettings
	}

	return ""
}

func providerAuthCredentialEnvironmentConfigured(options Options) bool {
	for _, name := range providerAuthCredentialEnvNames {
		if value, overridden := options.Env[name]; overridden {
			if strings.TrimSpace(value) != "" {
				return true
			}

			continue
		}

		if value, inherited := os.LookupEnv(name); inherited && strings.TrimSpace(value) != "" {
			return true
		}
	}

	return false
}

func providerAuthStaticSettingsConfigured(options Options, home providerAuthHome) bool {
	if home.err == nil && providerAuthSettingsFileConfigured(userSettingsPath(home.path)) {
		return true
	}

	if providerAuthSettingsFileConfigured(managedSettingsPath()) {
		return true
	}

	settingsFiles := map[string]struct{}{settingsFileName: {}}
	if options.SettingsFile != "" {
		settingsFiles[options.SettingsFile] = struct{}{}

		if home.err == nil && home.path != "" {
			path, err := resolveClaudeSettingsFile(home.path, options.SettingsFile)
			if err == nil && providerAuthSettingsFileConfigured(path) {
				return true
			}
		}
	}

	for path, content := range options.SeedFiles {
		if _, loaded := settingsFiles[path]; loaded && providerAuthSettingsContentConfigured([]byte(content)) {
			return true
		}
	}

	return false
}

func providerAuthSettingsFileConfigured(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	return providerAuthSettingsContentConfigured(content)
}

func providerAuthSettingsContentConfigured(content []byte) bool {
	var raw map[string]any
	if json.Unmarshal(content, &raw) != nil {
		return false
	}

	settings := decodeSettingsFile(context.Background(), raw, nil)
	if settings.APIKeyHelper != "" {
		return true
	}

	for _, name := range providerAuthCredentialEnvNames {
		if strings.TrimSpace(settings.Env[name]) != "" {
			return true
		}
	}

	return false
}

// providerAuthHome is the native config dir this surface acts on. It is
// resolved once, when the broker is built, and every later leg runs against
// this exact directory: re-deriving it per leg would let a name repointed while
// the agent runs inherit a consent decided over the directory it used to name.
type providerAuthHome struct {
	path     string
	identity os.FileInfo
	err      error
}

// resolveProviderAuthHome resolves the configured home and records which
// directory it named. An unset home resolves to the empty path the harness
// reads as its own default and carries no identity, so nothing consents to it.
func resolveProviderAuthHome(options Options) providerAuthHome {
	path, identity, err := resolveClaudeHome(options.Home)
	if err != nil {
		return providerAuthHome{err: err}
	}

	return providerAuthHome{path: path, identity: identity}
}

// unchanged reports whether the resolved path still names the directory
// resolution found there.
func (h providerAuthHome) unchanged() bool {
	if h.identity == nil {
		return false
	}

	identity, err := os.Stat(h.path)
	if err != nil {
		return false
	}

	return os.SameFile(identity, h.identity)
}

// providerAuthDirectHome reports whether the exact-home consent gate authorizes
// the home the broker resolved. Consent is granted over that resolved
// directory, so it covers exactly what a removal clears rather than a name that
// happens to point at it.
func providerAuthDirectHome(options Options, home providerAuthHome) bool {
	if strings.TrimSpace(options.ProviderAuthDirectHome) == "" || home.path == "" {
		return false
	}

	direct, err := canonicalClaudeHome(options.ProviderAuthDirectHome)
	if err != nil {
		return false
	}

	return direct == home.path
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
	return []string{
		AuthMethodsMethod,
		AuthAuthorizeMethod,
		AuthCallbackMethod,
		AuthStatusMethod,
		AuthCancelMethod,
		AuthInventoryMethod,
		AuthCredentialMethod,
		AuthDisconnectMethod,
	}
}

func (p *providerAuth) capability() map[string]any {
	return map[string]any{
		providerAuthMethodsField: p.authMethodNames(),
		providerAuthInjectionKey: providerAuthOptionPath,
	}
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
	if broker == nil {
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
	case AuthCredentialMethod:
		result, err := broker.credential(ctx, params)

		return result, true, err
	case AuthDisconnectMethod:
		result, err := broker.disconnect(ctx, params)

		return result, true, err
	default:
		return nil, false, nil
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
// tombstoned session gets the uniform unknown-session rejection, and so does one
// whose close already swept its flows: session/close terminalizes them before it
// drops the id from the live map, so between those two the id still resolves to
// a session that can no longer own a login.
func (p *providerAuth) authSession(id string) (*agentSession, error) {
	session, err := p.agent.session(acp.SessionId(id))
	if err != nil {
		return nil, err
	}

	if p.sessionClosed(session.id) {
		return nil, unknownSessionError()
	}

	return session, nil
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

// authConnectionIDMaxBytes bounds the caller-minted connection id. The value is
// durable — it lands in a ledger entry a later leg equality-checks against what
// the caller sent — and the bound leaves room for the opaque token a consumer
// mints, of which a prefixed UUID is forty bytes.
const authConnectionIDMaxBytes = 128

// authRequiredConnectionID decodes and validates the connection id a leg
// addresses. It runs where the value enters, ahead of every comparison and
// every write, so no leg ever fences against or records an id this bound
// refuses. The value is never normalised: a later leg compares it byte for byte
// with what the caller sent, so rewriting it would break that comparison.
func authRequiredConnectionID(fields map[string]json.RawMessage) (string, error) {
	value, err := authRequiredString(fields, authFieldConnectionID)
	if err != nil {
		return "", err
	}

	if !authValidConnectionID(value) {
		return "", invalidAuthField(authFieldConnectionID)
	}

	return value, nil
}

// authValidConnectionID reports whether id is an opaque bounded ASCII token.
// The alphabet keeps the id safe in every position it reaches — a path segment,
// a native label, and a log line — and admits no non-ASCII spelling, so two
// wire encodings can never decode to one Go string and alias one connection
// onto another's entry.
func authValidConnectionID(id string) bool {
	if id == "" || len(id) > authConnectionIDMaxBytes {
		return false
	}

	for index := range len(id) {
		if !authConnectionIDByte(id[index]) {
			return false
		}
	}

	return true
}

func authConnectionIDByte(char byte) bool {
	return (char >= 'A' && char <= 'Z') ||
		(char >= 'a' && char <= 'z') ||
		(char >= '0' && char <= '9') ||
		char == '-' || char == '_'
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
