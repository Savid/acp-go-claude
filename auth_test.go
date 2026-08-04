package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
// drivable without a Claude CLI. Accepting a value is the only thing in the
// fixture that puts a credential in the config dir, exactly as the real child
// is the only thing that does: a flow reporting a login the child never
// performed is reporting one that did not happen.
type fakeAuthLogin struct {
	seams *authTestSeams

	mu        sync.Mutex
	submitted []string
	closes    int
	exited    bool
	// refused makes the child reject the submitted value the way the provider
	// rejects a wrong or expired authorization code: the value crosses and the
	// config dir is left exactly as it was.
	refused     bool
	submitErr   error
	waitErr     error
	closeErr    error
	exitUnknown bool
	// beforeSubmit runs before the write is attempted and before the child's
	// own lock is taken, which is where anything racing the write lands: the
	// real child's stdin is closed by whoever terminalizes the flow, without
	// the broker mutex held.
	beforeSubmit func()
	beforeWait   func()
	beforeClose  func()
}

func (f *fakeAuthLogin) Submit(value string) error {
	if f.beforeSubmit != nil {
		f.beforeSubmit()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.submitted = append(f.submitted, value)

	if f.submitErr != nil {
		return f.submitErr
	}

	if !f.refused {
		f.seams.completeLogin()
	}

	return nil
}

func (f *fakeAuthLogin) Exited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.exited
}

func (f *fakeAuthLogin) Wait(ctx context.Context) (claude.AuthLoginExit, error) {
	if f.beforeWait != nil {
		f.beforeWait()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return claude.AuthLoginExitUnknown, err
	}
	if f.waitErr != nil {
		return claude.AuthLoginExitUnknown, f.waitErr
	}
	if f.exitUnknown {
		return claude.AuthLoginExitUnknown, nil
	}

	f.exited = true
	if f.refused {
		return claude.AuthLoginExitNonzero, nil
	}

	return claude.AuthLoginExitZero, nil
}

func (f *fakeAuthLogin) Close() error {
	if f.beforeClose != nil {
		f.beforeClose()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.closes++
	f.exited = true

	return f.closeErr
}

// markExited models the child ending on its own, which the harness's
// unconditionally armed loopback hook makes possible before any value is
// pasted back.
func (f *fakeAuthLogin) markExited() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.exited = true
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
	login        *fakeAuthLogin
	loginSession authLoginSession
	loginURL     string
	loginErr     error
	account      claude.AuthAccount
	statusExt    int
	statusErr    error
	logoutErr    error
	removeErr    error

	statusCalls int
	loginCalls  int
	logoutCalls int
	removeCalls int
	removedDir  string
	removedUser string
}

// completeLogin models the login child installing this login's credential in
// the config dir.
func (s *authTestSeams) completeLogin() {
	s.account = claude.AuthAccount{LoggedIn: true, AuthMethod: "oauth", Email: "owner@example.test"}
}

func newAuthSeams(t *testing.T) *authTestSeams {
	t.Helper()

	// The config dir starts empty. Nothing but an accepted login puts an account
	// in it, so a leg that reports a login has to have caused one.
	seams := &authTestSeams{
		login:    &fakeAuthLogin{},
		loginURL: "https://claude.com/oauth/authorize?redirect_uri=" + claude.AuthLoginRedirectURI,
	}
	seams.login.seams = seams

	beginOriginal := authLoginBegin
	statusOriginal := authStatusProbe
	logoutOriginal := authLogoutCommand
	removeOriginal := authKeychainRemove
	userOriginal := authNativeUser

	authLoginBegin = func(context.Context, claude.Options, *claude.DarwinGeneration) (authLoginSession, string, error) {
		seams.loginCalls++
		if seams.loginErr != nil {
			return nil, "", seams.loginErr
		}

		if seams.loginSession != nil {
			return seams.loginSession, seams.loginURL, nil
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

	authKeychainRemove = func(_ context.Context, configDir string, user string, _ claude.Options) error {
		seams.removeCalls++
		seams.removedDir = configDir
		seams.removedUser = user

		return seams.removeErr
	}

	authNativeUser = func(claude.Options) string { return "canary-user" }

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
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithLogger(slog.New(slog.DiscardHandler)),
		WithEnv(map[string]string{
			providerAuthEnvAnthropicAPIKey:  "",
			providerAuthEnvAnthropicToken:   "",
			providerAuthEnvClaudeOAuthToken: "",
		}),
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
		authFieldMethod:      authMethodLogin,
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

func TestProviderAuthUnadvertisedWithCredentialEnvironment(t *testing.T) {
	for _, name := range providerAuthCredentialEnvNames {
		t.Setenv(name, "")
	}

	for _, name := range providerAuthCredentialEnvNames {
		t.Run(name, func(t *testing.T) {
			agent := NewAgent(
				WithProviderAuthRoot(t.TempDir()),
				WithHome(t.TempDir()),
				WithEnv(map[string]string{name: "configured"}),
				WithLogger(slog.New(slog.DiscardHandler)),
			)
			require.Nil(t, agent.providerAuth)
		})
	}

	t.Setenv(providerAuthEnvClaudeOAuthToken, "inherited")
	require.True(t, providerAuthCredentialEnvironmentConfigured(Options{}))
	require.False(t, providerAuthCredentialEnvironmentConfigured(Options{
		Env: map[string]string{providerAuthEnvClaudeOAuthToken: ""},
	}))
}

func TestProviderAuthUnadvertisedWithAgentWideStaticAuthentication(t *testing.T) {
	for _, name := range providerAuthCredentialEnvNames {
		t.Setenv(name, "")
	}

	t.Run("bare mode", func(t *testing.T) {
		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(t.TempDir()),
			WithClaudeBareMode(true),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	t.Run("user settings environment", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(home, settingsFileName),
			[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"configured"}}`),
			0o600,
		))

		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(home),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	t.Run("api key helper", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(home, settingsFileName),
			[]byte(`{"apiKeyHelper":"/usr/local/bin/claude-key"}`),
			0o600,
		))

		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(home),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	t.Run("seeded settings", func(t *testing.T) {
		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(t.TempDir()),
			WithSeedFiles(map[string]string{
				settingsFileName: `{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"configured"}}`,
			}),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	t.Run("settings overlay", func(t *testing.T) {
		home := t.TempDir()
		const overlay = "worker.settings.json"
		require.NoError(t, os.WriteFile(
			filepath.Join(home, overlay),
			[]byte(`{"env":{"ANTHROPIC_API_KEY":"configured"}}`),
			0o600,
		))

		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(home),
			WithClaudeSettingsFile(overlay),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	t.Run("managed settings", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "managed-settings.json")
		require.NoError(t, os.WriteFile(
			path,
			[]byte(`{"apiKeyHelper":"/usr/local/bin/managed-claude-key"}`),
			0o600,
		))

		original := managedSettingsPath
		managedSettingsPath = func() string { return path }
		t.Cleanup(func() { managedSettingsPath = original })

		agent := NewAgent(
			WithProviderAuthRoot(t.TempDir()),
			WithHome(t.TempDir()),
			WithLogger(slog.New(slog.DiscardHandler)),
		)
		require.Nil(t, agent.providerAuth)
	})

	require.False(t, providerAuthSettingsContentConfigured([]byte("{")))
}

func TestProviderAuthSuppressionLogsInfoWithoutTheCredential(t *testing.T) {
	for _, name := range providerAuthCredentialEnvNames {
		t.Setenv(name, "")
	}

	const credential = "must-not-appear"

	var logs bytes.Buffer
	agent := NewAgent(
		WithProviderAuthRoot(t.TempDir()),
		WithHome(t.TempDir()),
		WithEnv(map[string]string{providerAuthEnvClaudeOAuthToken: credential}),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
	)
	require.Nil(t, agent.providerAuth)
	logged := logs.String()
	require.Contains(t, logged, "level=INFO")
	require.Contains(t, logged, "interactive provider auth disabled")
	require.Contains(t, logged, "reason=\""+providerAuthUnavailableEnv+"\"")
	require.NotContains(t, logged, "level=WARN")
	require.NotContains(t, logged, "error=")
	require.NotContains(t, logged, credential)
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

	gate := func(options Options) bool {
		return providerAuthDirectHome(options, resolveProviderAuthHome(options))
	}

	require.False(t, gate(Options{}))
	require.False(t, gate(Options{Home: home}))
	require.False(t, gate(Options{Home: home, ProviderAuthDirectHome: filepath.Dir(home)}))
	require.True(t, gate(Options{Home: home, ProviderAuthDirectHome: home + "/."}))

	// The gate answers about the directory the leg clears, which is the
	// resolved one: a link and its target are the same consented home, and a
	// home that resolves to nothing is consented to by neither spelling.
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(home, link))
	require.True(t, gate(Options{Home: link, ProviderAuthDirectHome: home}))
	require.True(t, gate(Options{Home: home, ProviderAuthDirectHome: link}))

	absent := filepath.Join(t.TempDir(), "absent")
	require.False(t, gate(Options{Home: absent, ProviderAuthDirectHome: absent}))
	require.False(t, gate(Options{Home: home, ProviderAuthDirectHome: absent}))
	require.False(t, gate(Options{Home: absent, ProviderAuthDirectHome: home}))

	// Resolution records the directory it found, so a name that no longer
	// reaches it is not the home consent was granted over.
	resolved := resolveProviderAuthHome(Options{Home: home})
	require.True(t, resolved.unchanged())
	require.False(t, providerAuthHome{}.unchanged())

	require.NoError(t, os.Rename(home, home+"-moved"))
	require.False(t, resolved.unchanged())
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
		AuthCredentialMethod,
		AuthDisconnectMethod,
	}, broker.authMethodNames())

	meta := broker.agent.capabilityMeta()
	vendor, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)

	capability, ok := vendor[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, broker.authMethodNames(), capability[providerAuthMethodsField])
	require.Equal(t, providerAuthOptionPath, capability[providerAuthInjectionKey])
}

func TestProviderAuthDisconnectAdvertisedUnderTheConsentGate(t *testing.T) {
	home := t.TempDir()
	broker, _ := newAuthBroker(t, WithHome(home), WithProviderAuthDirectHome(home))

	require.Contains(t, broker.authMethodNames(), AuthDisconnectMethod)
	require.True(t, broker.advertises(AuthDisconnectMethod))
	require.True(t, broker.advertises(AuthCredentialMethod))
	require.False(t, broker.advertises("_claude/auth/unknown"))
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
		authFieldMethod: authMethodLogin,
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
	failure := &authFailedError{cause: authCauseTransport, providerID: authProviderID, method: authMethodLogin, flowID: "flow"}
	require.Equal(t, "claude_auth_failed: transport", failure.Error())

	data, ok := failure.requestError().Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, authFailedErrorTag, data[jsonFieldError])
	require.Equal(t, true, data[authFieldRetryable])
	require.Equal(t, authProviderID, data[authFieldProviderID])
	require.Equal(t, authMethodLogin, data[authFieldMethod])
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
		authCauseFlowState, authCauseFlowCancelled,
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
		{authCauseFlowCancelled, false, "", ""},
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

// adversarialConnectionIDs are the caller-minted values the bound refuses. Each
// is a shape the id would otherwise carry into a durable ledger entry and into
// the adapter's own logs, and the two replacement-rune spellings are one Go
// string reached from two different wire encodings, which aliases one
// connection onto another's entry.
func adversarialConnectionIDs() map[string]string {
	return map[string]string{
		"empty":              "",
		"path separators":    "../../../etc/passwd",
		"windows separators": `..\..\connection`,
		"newline":            "connection\n1",
		"nul":                "connection\x00 1",
		"bidi override":      "connection\u202e1",
		"space":              "connection 1",
		"colon":              "connection:1",
		"replacement rune":   "connection-�",
		"non ascii":          "connection-é",
		"unbounded":          strings.Repeat("c", authConnectionIDMaxBytes+1),
	}
}

func TestConnectionIDIsRefusedAtEverySurfaceEntry(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newDisconnectBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	require.NoError(t, broker.ledger.write(authLedgerRecord{
		ProviderID:        authProviderID,
		Method:            authMethodLogin,
		ConnectionID:      testConnectionID,
		BindingGeneration: 1,
		State:             authLedgerConfirmed,
	}))

	for name, connectionID := range adversarialConnectionIDs() {
		t.Run(name, func(t *testing.T) {
			authorize := authorizeParams(sessionID, generation)
			authorize["connectionId"] = connectionID

			_, err := broker.authorize(t.Context(), authParams(t, authorize))
			requireInvalidAuthField(t, err, authFieldConnectionID)

			disconnect := disconnectParams(sessionID, 1)
			disconnect["connectionId"] = connectionID

			_, err = broker.disconnect(t.Context(), authParams(t, disconnect))
			requireInvalidAuthField(t, err, authFieldConnectionID)
		})
	}

	// Every refusal above landed before the leg touched the entry the live
	// binding names.
	live, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1), live.BindingGeneration)
	require.Equal(t, testConnectionID, live.ConnectionID)
}

func TestConnectionIDAcceptsTheOpaqueTokenAConsumerMints(t *testing.T) {
	for _, connectionID := range []string{
		"pac_2f1c9b4e-8d3a-4c17-9f21-0b6e5a7c8d90",
		testConnectionID,
		"C0",
		strings.Repeat("c", authConnectionIDMaxBytes),
	} {
		require.True(t, authValidConnectionID(connectionID), connectionID)
	}
}
