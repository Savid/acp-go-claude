package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

const (
	testSessionID    = "session-1"
	testConnectionID = "connection-1"
	testRequestID    = "request-1"
	testPastedValue  = "code-half#state-half"
)

// fakeAuthLogin stands in for the running login child so every flow path is
// drivable without a Claude CLI.
type fakeAuthLogin struct {
	mu        sync.Mutex
	submitted []string
	closes    int
	exited    bool
	submitErr error
	closeErr  error
}

func (f *fakeAuthLogin) Submit(value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.submitted = append(f.submitted, value)

	return f.submitErr
}

func (f *fakeAuthLogin) Exited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.exited
}

func (f *fakeAuthLogin) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closes++
	f.exited = true

	return f.closeErr
}

func (f *fakeAuthLogin) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closes
}

func (f *fakeAuthLogin) values() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.submitted...)
}

// authTestSeams swaps every native seam for a deterministic one and restores
// the originals when the test ends.
type authTestSeams struct {
	login     *fakeAuthLogin
	loginURL  string
	loginErr  error
	account   claude.AuthAccount
	statusExt int
	statusErr error
	logoutErr error
	removeErr error

	statusCalls int
	logoutCalls int
	removeCalls int
	removedDir  string
	removedUser string
}

func newAuthSeams(t *testing.T) *authTestSeams {
	t.Helper()

	seams := &authTestSeams{
		login:    &fakeAuthLogin{},
		loginURL: "https://claude.com/oauth/authorize?redirect_uri=" + claude.AuthLoginRedirectURI,
		account:  claude.AuthAccount{LoggedIn: true},
	}

	beginOriginal := authLoginBegin
	statusOriginal := authStatusProbe
	logoutOriginal := authLogoutCommand
	removeOriginal := authKeychainRemove
	userOriginal := authNativeUser

	authLoginBegin = func(context.Context, claude.Options, *claude.DarwinGeneration) (authLoginSession, string, error) {
		if seams.loginErr != nil {
			return nil, "", seams.loginErr
		}

		return seams.login, seams.loginURL, nil
	}

	authStatusProbe = func(context.Context, claude.Options, *claude.DarwinGeneration) (claude.AuthAccount, int, error) {
		seams.statusCalls++

		return seams.account, seams.statusExt, seams.statusErr
	}

	authLogoutCommand = func(context.Context, claude.Options, *claude.DarwinGeneration) (int, error) {
		seams.logoutCalls++

		return 0, seams.logoutErr
	}

	authKeychainRemove = func(_ context.Context, configDir string, user string) error {
		seams.removeCalls++
		seams.removedDir = configDir
		seams.removedUser = user

		return seams.removeErr
	}

	authNativeUser = func() string { return "canary-user" }

	t.Cleanup(func() {
		authLoginBegin = beginOriginal
		authStatusProbe = statusOriginal
		authLogoutCommand = logoutOriginal
		authKeychainRemove = removeOriginal
		authNativeUser = userOriginal
	})

	return seams
}

// newAuthAgent builds an agent whose provider-auth surface is advertised and
// whose native admission is deterministic on every platform.
func newAuthAgent(t *testing.T, opts ...Option) *Agent {
	t.Helper()

	options := append([]Option{
		WithProviderAuthRoot(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithLogger(slog.New(slog.DiscardHandler)),
	}, opts...)

	agent := NewAgent(options...)
	agent.containmentMode = RuntimeContainmentAuthoritative

	return agent
}

func newAuthBroker(t *testing.T, opts ...Option) (*providerAuth, acp.SessionId) {
	t.Helper()

	agent := newAuthAgent(t, opts...)
	require.NotNil(t, agent.providerAuth)

	session := &agentSession{agent: agent, id: testSessionID}
	agent.sessions[session.id] = session

	return agent.providerAuth, session.id
}

// authCatalogGeneration primes the broker's catalog and returns the token an
// authorize must present back.
func authCatalogGeneration(t *testing.T, broker *providerAuth, sessionID acp.SessionId) string {
	t.Helper()

	result, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)

	methods, ok := result.(authMethodsResult)
	require.True(t, ok)

	return methods.Generation
}

func authParams(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func authorizeParams(sessionID acp.SessionId, generation string) map[string]any {
	return map[string]any{
		"sessionId":          string(sessionID),
		"providerId":         authProviderID,
		"connectionId":       testConnectionID,
		"methodsGeneration":  generation,
		authFieldMethod:      authMethodID,
		"authorizeRequestId": testRequestID,
	}
}

// startAuthFlow drives methods plus authorize and returns the minted flow.
func startAuthFlow(t *testing.T, broker *providerAuth, sessionID acp.SessionId) authAuthorizeResult {
	t.Helper()

	generation := authCatalogGeneration(t, broker, sessionID)

	result, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)

	flow, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	return flow
}

func requireAuthFailed(t *testing.T, err error, cause string) {
	t.Helper()

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32000, requestErr.Code)

	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, authFailedErrorTag, data[jsonFieldError])
	require.Equal(t, cause, data[failureFieldCause])
}

func requireInvalidAuthField(t *testing.T, err error, field string) {
	t.Helper()

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)

	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, field, data[jsonFieldField])
}

func TestProviderAuthUnadvertisedWithoutRoot(t *testing.T) {
	agent := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))
	require.Nil(t, agent.providerAuth)

	meta := agent.capabilityMeta()
	vendor, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, vendor, providerAuthCapabilityKey)

	_, err := agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, nil)

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32601, requestErr.Code)
}

func TestProviderAuthUnusableRootStaysUnadvertised(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	agent := NewAgent(WithProviderAuthRoot(file), WithLogger(slog.New(slog.DiscardHandler)))
	require.Nil(t, agent.providerAuth)
}

func TestProviderAuthRelativePathsFailConstruction(t *testing.T) {
	require.Error(t, validateProviderAuthRoot(Options{ProviderAuthRoot: "relative"}))
	require.NoError(t, validateProviderAuthRoot(Options{ProviderAuthRoot: "/abs"}))
	require.NoError(t, validateProviderAuthRoot(Options{}))

	require.Error(t, validateProviderAuthDirectHome("relative"))
	require.NoError(t, validateProviderAuthDirectHome("/abs"))
	require.NoError(t, validateProviderAuthDirectHome(""))

	agent := NewAgent(WithProviderAuthRoot("relative"), WithLogger(slog.New(slog.DiscardHandler)))
	require.Nil(t, agent.providerAuth)

	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.Error(t, err)
}

func TestProviderAuthDirectHomeGate(t *testing.T) {
	home := t.TempDir()

	require.False(t, providerAuthDirectHome(Options{}))
	require.False(t, providerAuthDirectHome(Options{Home: home}))
	require.False(t, providerAuthDirectHome(Options{Home: home, ProviderAuthDirectHome: filepath.Dir(home)}))
	require.True(t, providerAuthDirectHome(Options{Home: home, ProviderAuthDirectHome: home + "/."}))

	// The gate answers about the directory the leg clears, which is the
	// resolved one: a link and its target are the same consented home, and a
	// home that resolves to nothing is consented to by neither spelling.
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(home, link))
	require.True(t, providerAuthDirectHome(Options{Home: link, ProviderAuthDirectHome: home}))
	require.True(t, providerAuthDirectHome(Options{Home: home, ProviderAuthDirectHome: link}))

	absent := filepath.Join(t.TempDir(), "absent")
	require.False(t, providerAuthDirectHome(Options{Home: absent, ProviderAuthDirectHome: absent}))
	require.False(t, providerAuthDirectHome(Options{Home: home, ProviderAuthDirectHome: absent}))
	require.False(t, providerAuthDirectHome(Options{Home: absent, ProviderAuthDirectHome: home}))
}

func TestProviderAuthCapabilityListsExactlyTheEnabledLegs(t *testing.T) {
	broker, _ := newAuthBroker(t)
	require.Equal(t, []string{
		AuthMethodsMethod,
		AuthAuthorizeMethod,
		AuthCallbackMethod,
		AuthStatusMethod,
		AuthCancelMethod,
		AuthInventoryMethod,
	}, broker.authMethodNames())

	meta := broker.agent.capabilityMeta()
	vendor, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)

	capability, ok := vendor[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, broker.authMethodNames(), capability[providerAuthMethodsField])
	require.NotContains(t, capability, "injectionKey")

	_, err := broker.agent.HandleExtensionMethod(t.Context(), AuthDisconnectMethod, nil)

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32601, requestErr.Code)
}

func TestProviderAuthDisconnectAdvertisedUnderTheConsentGate(t *testing.T) {
	home := t.TempDir()
	broker, _ := newAuthBroker(t, WithHome(home), WithProviderAuthDirectHome(home))

	require.Contains(t, broker.authMethodNames(), AuthDisconnectMethod)
	require.True(t, broker.advertises(AuthDisconnectMethod))
	require.False(t, broker.advertises("_claude/auth/credential"))
}

func TestProviderAuthInjectionOptionFailsClosed(t *testing.T) {
	_, err := claudeOptionsFromMeta(map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{"providerAuth": map[string]any{}},
		},
	})
	requireInvalidAuthField(t, err, "_meta.claude.options.providerAuth")
}

func TestProviderAuthDispatchRoutesEveryAdvertisedLeg(t *testing.T) {
	newAuthSeams(t)

	home := t.TempDir()
	broker, sessionID := newAuthBroker(t, WithHome(home), WithProviderAuthDirectHome(home))
	agent := broker.agent

	sessionParams := authParams(t, map[string]any{"sessionId": string(sessionID)})

	enumerated, err := agent.HandleExtensionMethod(t.Context(), AuthMethodsMethod, sessionParams)
	require.NoError(t, err)

	catalog, ok := enumerated.(authMethodsResult)
	require.True(t, ok)

	authorized, err := agent.HandleExtensionMethod(t.Context(), AuthAuthorizeMethod,
		authParams(t, authorizeParams(sessionID, catalog.Generation)))
	require.NoError(t, err)

	flow, isAuthorize := authorized.(authAuthorizeResult)
	require.True(t, isAuthorize)

	flowParams := authParams(t, map[string]any{
		"sessionId":  string(sessionID),
		"providerId": authProviderID,
		"flowId":     flow.FlowID,
	})

	_, err = agent.HandleExtensionMethod(t.Context(), AuthStatusMethod, flowParams)
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(t.Context(), AuthInventoryMethod, sessionParams)
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(t.Context(), AuthCallbackMethod, authParams(t, map[string]any{
		"sessionId":     string(sessionID),
		"providerId":    authProviderID,
		authFieldMethod: authMethodID,
		"flowId":        flow.FlowID,
		"input":         testPastedValue,
	}))
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(t.Context(), AuthCancelMethod, flowParams)
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(t.Context(), AuthDisconnectMethod, authParams(t, map[string]any{
		"sessionId":         string(sessionID),
		"providerId":        authProviderID,
		"connectionId":      testConnectionID,
		"bindingGeneration": 1,
	}))
	requireAuthFailed(t, err, authCauseHarvestFailed)
}

func TestAuthFailedErrorCarriesTheClosedShape(t *testing.T) {
	failure := &authFailedError{cause: authCauseTransport, providerID: authProviderID, method: authMethodID, flowID: "flow"}
	require.Equal(t, "claude_auth_failed: transport", failure.Error())

	data, ok := failure.requestError().Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, authFailedErrorTag, data[jsonFieldError])
	require.Equal(t, true, data[authFieldRetryable])
	require.Equal(t, authProviderID, data[authFieldProviderID])
	require.Equal(t, authMethodID, data[authFieldMethod])
	require.Equal(t, "flow", data[authFieldFlowID])

	bare := &authFailedError{cause: authCauseNativeVeto}

	bareData, ok := bare.requestError().Data.(map[string]any)
	require.True(t, ok)
	require.NotContains(t, bareData, authFieldProviderID)
	require.NotContains(t, bareData, authFieldMethod)
	require.NotContains(t, bareData, authFieldFlowID)
	require.Equal(t, false, bareData[authFieldRetryable])
}

func TestAuthCauseRetryable(t *testing.T) {
	for _, cause := range []string{authCauseTransport, authCauseProcess, authCauseTimeout} {
		require.True(t, authCauseRetryable(cause))
	}

	for _, cause := range []string{
		authCauseNativeVeto, authCauseProviderRefused, authCausePolicy, authCauseBindingConflict,
	} {
		require.False(t, authCauseRetryable(cause))
	}
}

func TestAuthFlowTransitionIsTotalOverTheClosedCauses(t *testing.T) {
	cases := []struct {
		cause    string
		inFlight bool
		state    string
		reason   string
	}{
		{authCauseNativeVeto, false, authStateFailed, authReasonNativeVeto},
		{authCauseUnsupportedVariant, false, authStateFailed, authReasonNativeVeto},
		{authCauseProviderRefused, false, authStateFailed, authReasonProviderRefused},
		{authCauseTransport, false, authStateFailed, authReasonTransport},
		{authCauseTransport, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseProcess, false, authStateFailed, authReasonProcess},
		{authCauseProcess, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseTimeout, false, authStateFailed, authReasonTransport},
		{authCauseTimeout, true, authStateFailed, authReasonAcceptanceUnknown},
		{authCauseHarvestFailed, false, authStateFailed, authReasonHarvestFailed},
		{authCauseFlowExpired, false, authStateExpired, authReasonDeadline},
		{authCausePolicy, false, "", ""},
		{authCauseBindingConflict, false, "", ""},
		{authCauseFlowState, false, "", ""},
	}

	for _, testCase := range cases {
		state, reason := authFlowTransition(testCase.cause, testCase.inFlight)
		require.Equal(t, testCase.state, state, testCase.cause)
		require.Equal(t, testCase.reason, reason, testCase.cause)
	}
}

func TestAuthParamFieldsRejectsEveryMalformedBody(t *testing.T) {
	_, err := authParamFields(json.RawMessage(`[]`), "sessionId")
	requireInvalidAuthField(t, err, authFieldParams)

	_, err = authParamFields(json.RawMessage(`{"other":1}`), "sessionId")
	requireInvalidAuthField(t, err, "other")

	_, err = authParamFields(json.RawMessage(`{"sessionId":"a","sessionId":"b"}`), "sessionId")
	requireInvalidAuthField(t, err, "sessionId")

	_, err = authParamFields(json.RawMessage(`{"sessionId":`), "sessionId")
	requireInvalidAuthField(t, err, "sessionId")

	_, err = authParamFields(json.RawMessage(`{"sessionId":"a"`), "sessionId")
	requireInvalidAuthField(t, err, authFieldParams)

	_, err = authParamFields(json.RawMessage(`{"sessionId":"a"} []`), "sessionId")
	requireInvalidAuthField(t, err, authFieldParams)

	_, err = authParamFields(json.RawMessage(`{"sessionId":"a",}`), "sessionId")
	requireInvalidAuthField(t, err, authFieldParams)

	fields, err := authParamFields(json.RawMessage(`{"sessionId":"a"}`), "sessionId")
	require.NoError(t, err)
	require.Len(t, fields, 1)
}

func TestAuthFieldDecoders(t *testing.T) {
	fields := map[string]json.RawMessage{
		"present": json.RawMessage(`"value"`),
		"empty":   json.RawMessage(`""`),
		"wrong":   json.RawMessage(`5`),
		"number":  json.RawMessage(`7`),
	}

	value, err := authRequiredString(fields, "present")
	require.NoError(t, err)
	require.Equal(t, "value", value)

	_, err = authRequiredString(fields, "empty")
	requireInvalidAuthField(t, err, "empty")

	_, err = authRequiredString(fields, "absent")
	requireInvalidAuthField(t, err, "absent")

	empty, err := authString(fields, "empty")
	require.NoError(t, err)
	require.Empty(t, empty)

	_, err = authString(fields, "wrong")
	requireInvalidAuthField(t, err, "wrong")

	_, err = authString(fields, "absent")
	requireInvalidAuthField(t, err, "absent")

	number, err := authRequiredInt64(fields, "number")
	require.NoError(t, err)
	require.Equal(t, int64(7), number)

	_, err = authRequiredInt64(fields, "present")
	requireInvalidAuthField(t, err, "present")

	_, err = authRequiredInt64(fields, "absent")
	requireInvalidAuthField(t, err, "absent")
}

func TestProviderAuthGoSafeRecoversPanics(t *testing.T) {
	broker, _ := newAuthBroker(t)
	done := make(chan struct{})

	broker.goSafe(func() {
		defer close(done)

		panic(errors.New("boom"))
	})

	<-done
}

var errTestRandom = errors.New("no entropy")
