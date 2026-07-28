package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func flowParams(sessionID, flowID string) map[string]any {
	return map[string]any{
		"sessionId":  sessionID,
		"providerId": authProviderID,
		"flowId":     flowID,
	}
}

func callbackParams(sessionID, flowID, input string) map[string]any {
	return map[string]any{
		"sessionId":     sessionID,
		"providerId":    authProviderID,
		authFieldMethod: authMethodID,
		"flowId":        flowID,
		"input":         input,
	}
}

func TestAuthorizeReturnsTheHostedPasteBackPresentation(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	require.Equal(t, authInteractionCallback, flow.Interaction)
	require.Equal(t, authCallbackInputCode, flow.CallbackInput)
	require.Equal(t, seams.loginURL, flow.URL)
	require.Equal(t, authMethodLabel, flow.Message)
	require.NotEmpty(t, flow.FlowID)
	require.Positive(t, flow.FlowExpiresAt)

	// No user code crosses: nothing machine-readable carries one, and it is
	// never parsed out of the message.
	encoded, err := json.Marshal(flow)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "userCode")
	require.NotContains(t, string(encoded), "pollIntervalMs")
}

func TestAuthorizeRejectsAddressingAndInputFailures(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	_, err := broker.authorize(t.Context(), json.RawMessage(`{"extra":1}`))
	requireInvalidAuthField(t, err, "extra")

	params := authorizeParams(sessionID, generation)
	delete(params, "connectionId")

	_, err = broker.authorize(t.Context(), authParams(t, params))
	requireInvalidAuthField(t, err, "connectionId")

	stale := authorizeParams(sessionID, "not-the-current-token")

	_, err = broker.authorize(t.Context(), authParams(t, stale))
	requireInvalidAuthField(t, err, "methodsGeneration")

	wrongMethod := authorizeParams(sessionID, generation)
	wrongMethod[authFieldMethod] = "not-a-method"

	_, err = broker.authorize(t.Context(), authParams(t, wrongMethod))
	requireInvalidAuthField(t, err, authFieldMethod)

	badInputs := authorizeParams(sessionID, generation)
	badInputs["inputs"] = map[string]any{"instanceUrl": "https://evil.example"}

	_, err = broker.authorize(t.Context(), authParams(t, badInputs))
	requireInvalidAuthField(t, err, "inputs")

	nonStringInputs := authorizeParams(sessionID, generation)
	nonStringInputs["inputs"] = []string{"a"}

	_, err = broker.authorize(t.Context(), authParams(t, nonStringInputs))
	requireInvalidAuthField(t, err, "inputs")

	unknownSession := authorizeParams("missing", generation)

	_, err = broker.authorize(t.Context(), authParams(t, unknownSession))
	requireInvalidAuthField(t, err, "sessionId")
}

func TestAuthorizeReplaysTheRecordedPresentationVerbatim(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	first := startAuthFlow(t, broker, sessionID)
	generation := broker.generation

	replayed, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	require.Zero(t, seams.login.closeCount())
}

// TestAuthorizeReplaysTheRecordedPresentationAfterTheFlowTerminalized pins the
// whole point of the idempotency key: the repeat that matters is the one a
// caller sends after the first answer was lost, which is exactly when the flow
// it names has already completed.
func TestAuthorizeReplaysTheRecordedPresentationAfterTheFlowTerminalized(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	first := startAuthFlow(t, broker, sessionID)
	generation := broker.generation

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), first.FlowID, testPastedValue)))
	require.NoError(t, err)

	record, found, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, found)

	closes, statusCalls := seams.login.closeCount(), seams.statusCalls

	replayed, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)
	require.Equal(t, first, replayed)

	// No supersede, no fresh login child, no native read, and no ledger
	// revision: the repeat consumed nothing the first call had earned.
	require.Equal(t, closes, seams.login.closeCount())
	require.Equal(t, statusCalls, seams.statusCalls)

	after, _, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.Equal(t, record, after)

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateAuthenticated, reported.State)
}

// TestAuthorizeStopsReplayingOnceTheSessionCloses pins the other half: the
// record lives exactly as long as the session that owns it.
func TestAuthorizeStopsReplayingOnceTheSessionCloses(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)

	first := startAuthFlow(t, broker, sessionID)
	generation := broker.generation

	broker.closeSession(sessionID)

	result, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)

	minted, ok := result.(authAuthorizeResult)
	require.True(t, ok)
	require.NotEqual(t, first.FlowID, minted.FlowID)
}

// TestAuthorizeMintFailureAddressesTheFlowItNames pins the flowId a failed mint
// returns against a record a caller can actually address, and pins that the
// same key retries rather than replaying a presentation that was never
// published.
func TestAuthorizeMintFailureAddressesTheFlowItNames(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	seams.loginErr = errors.New("spawn failed")

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseProcess)

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)

	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)

	flowID, ok := data[authFieldFlowID].(string)
	require.True(t, ok)
	require.NotEmpty(t, flowID)

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flowID)))
	require.NoError(t, err)

	reported, isStatus := status.(authStatusResult)
	require.True(t, isStatus)
	require.Equal(t, authStateFailed, reported.State)
	require.Equal(t, authReasonProcess, reported.Reason)

	seams.loginErr = nil

	retried, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)

	minted, isResult := retried.(authAuthorizeResult)
	require.True(t, isResult)
	require.NotEqual(t, flowID, minted.FlowID)
	require.NotEmpty(t, minted.URL)
}

func TestAuthorizeSupersedesTheOlderFlow(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	first := startAuthFlow(t, broker, sessionID)

	params := authorizeParams(sessionID, broker.generation)
	params["authorizeRequestId"] = "request-2"

	second, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)

	superseded, ok := second.(authAuthorizeResult)
	require.True(t, ok)
	require.NotEqual(t, first.FlowID, superseded.FlowID)
	require.Equal(t, 1, seams.login.closeCount())

	// The superseded flowId addresses nothing on every leg that takes one.
	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)

	_, err = broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)

	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), first.FlowID, testPastedValue)))
	requireInvalidAuthField(t, err, authFieldFlowID)
}

// TestAuthorizeSupersedesAFlowThatAlreadyTerminalized pins the half a terminal
// state used to skip. A flow stays addressable through every transition of its
// own, but being replaced ends its life, and after that its id addresses
// nothing whatever state it ended on.
func TestAuthorizeSupersedesAFlowThatAlreadyTerminalized(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	first := startAuthFlow(t, broker, sessionID)

	seams.login.refused = true

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), first.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseProviderRefused)

	// Terminal, and still addressable until something replaces it.
	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateFailed, reported.State)

	params := authorizeParams(sessionID, broker.generation)
	params["authorizeRequestId"] = "request-2"

	second, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)

	minted, isResult := second.(authAuthorizeResult)
	require.True(t, isResult)
	require.NotEqual(t, first.FlowID, minted.FlowID)

	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)

	_, err = broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)

	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), first.FlowID, testPastedValue)))
	requireInvalidAuthField(t, err, authFieldFlowID)

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Len(t, broker.byID, 1)
	require.Contains(t, broker.byID, minted.FlowID)
}

// TestCloseSessionDropsEveryAddressingEntry pins the addressing map against the
// flow map. A minted id that outlives its session is a record nothing can ever
// reach and nothing ever frees.
func TestCloseSessionDropsEveryAddressingEntry(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	terminal := startAuthFlow(t, broker, sessionID)

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), terminal.FlowID, testPastedValue)))
	require.NoError(t, err)

	params := authorizeParams(sessionID, broker.generation)
	params["authorizeRequestId"] = "request-2"

	result, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)

	pending, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	closes := seams.login.closeCount()

	broker.closeSession(sessionID)

	broker.mu.Lock()
	require.Empty(t, broker.flows)
	require.Empty(t, broker.byID)
	broker.mu.Unlock()

	// Only the flow still pending owns a live child; the terminal one was
	// fenced when it terminalized.
	require.Equal(t, closes+1, seams.login.closeCount())

	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), pending.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)
}

func TestAuthorizeFailsClosedOnEveryNativeRefusal(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	seams.loginErr = claude.ErrAuthLoginGrammar

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseNativeVeto)

	seams.loginErr = claude.ErrAuthLoginNoURL

	_, err = broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseNativeVeto)

	seams.loginErr = errors.New("spawn failed")

	_, err = broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseProcess)

	seams.loginErr = nil
	seams.loginURL = "http://claude.com/oauth"

	_, err = broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseNativeVeto)
	require.Equal(t, 1, seams.login.closeCount())
}

func TestAuthorizeFailsClosedWhenTheMessageBoundIsViolated(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	broker.mu.Lock()
	broker.catalog = map[string][]authCatalogMethod{authProviderID: {{
		ID:          authMethodID,
		Type:        authMethodTypeOAuth,
		Label:       strings.Repeat("a", authMaxMessageBytes+1),
		Interaction: authInteractionCallback,
	}}}
	broker.mu.Unlock()

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseNativeVeto)
}

func TestAuthorizeFailsClosedWhenNoFlowTokenCanBeMinted(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	original := authRandRead

	authRandRead = func([]byte) (int, error) { return 0, errTestRandom }

	t.Cleanup(func() { authRandRead = original })

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestCallbackCompletesTheFlowFromTheStatusExitCode(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	result, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, result)
	require.Equal(t, []string{testPastedValue}, seams.login.values())

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateAuthenticated, reported.State)
	require.Empty(t, reported.Reason)

	record, found, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, authLedgerConfirmed, record.State)

	// A second callback addresses a real flow that no longer accepts input.
	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseFlowState)
}

// TestCallbackRefusesAValueTheProviderRejectedOnAPopulatedConfigDir pins the
// completion signal against the flow rather than against the config dir. The
// config dir already holds a credential and the pasted value changes nothing:
// `auth status` answers logged-in either way, so a leg reading only the current
// state binds the new connection to the credential that was already there.
func TestCallbackRefusesAValueTheProviderRejectedOnAPopulatedConfigDir(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	seams.account = claude.AuthAccount{LoggedIn: true, AuthMethod: "oauth", Email: "resident@example.test"}

	flow := startAuthFlow(t, broker, sessionID)

	seams.login.refused = true

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseProviderRefused)

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateFailed, reported.State)
	require.Equal(t, authReasonProviderRefused, reported.Reason)

	// The refused flow never earned a confirmation, so the connection it names
	// stays unproven instead of inheriting the resident credential.
	record, found, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, authLedgerIntent, record.State)
}

// TestStatusProbeRefusesAConfigDirThatDidNotChange pins the same rule on the
// backstop the status poll drives: a login child that exited without completing
// the exchange leaves the resident credential exactly where it was.
func TestStatusProbeRefusesAConfigDirThatDidNotChange(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	seams.account = claude.AuthAccount{LoggedIn: true, AuthMethod: "oauth", Email: "resident@example.test"}

	flow := startAuthFlow(t, broker, sessionID)

	seams.login.exited = true

	broker.mu.Lock()
	broker.byID[flow.FlowID].nextProbeAt = time.Time{}
	broker.mu.Unlock()

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStatePending, reported.State)
}

func TestCallbackRejectsMalformedPastedValues(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	for _, input := range []string{
		"",
		strings.Repeat("a", authMaxTextInputBytes+1),
		string([]byte{0xff}),
		"code\nstate",
		"code\x01state",
		"bare-code",
		"#state",
		"code#",
		"code#state#extra",
	} {
		_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, input)))
		requireInvalidAuthField(t, err, "input")
	}
}

func TestCallbackRejectsAddressingFailures(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	_, err := broker.callback(t.Context(), json.RawMessage(`{"nope":1}`))
	requireInvalidAuthField(t, err, "nope")

	for _, missing := range []string{"sessionId", "providerId", authFieldMethod, "flowId", "input"} {
		params := callbackParams(string(sessionID), flow.FlowID, testPastedValue)
		delete(params, missing)

		_, missingErr := broker.callback(t.Context(), authParams(t, params))
		requireInvalidAuthField(t, missingErr, missing)
	}

	_, err = broker.callback(t.Context(), authParams(t, callbackParams("missing", flow.FlowID, testPastedValue)))
	requireInvalidAuthField(t, err, "sessionId")

	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), "not-a-flow", testPastedValue)))
	requireInvalidAuthField(t, err, "flowId")

	wrongMethod := callbackParams(string(sessionID), flow.FlowID, testPastedValue)
	wrongMethod[authFieldMethod] = "other"

	_, err = broker.callback(t.Context(), authParams(t, wrongMethod))
	requireInvalidAuthField(t, err, authFieldMethod)
}

func TestCallbackFailureMapping(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*authTestSeams)
		cause   string
		reason  string
	}{
		{
			name:    "submit fails after the value crossed",
			arrange: func(seams *authTestSeams) { seams.login.submitErr = errors.New("pipe closed") },
			cause:   authCauseProcess,
			reason:  authReasonAcceptanceUnknown,
		},
		{
			name:    "the completion probe cannot run",
			arrange: func(seams *authTestSeams) { seams.statusErr = errors.New("probe failed") },
			cause:   authCauseProcess,
			reason:  authReasonAcceptanceUnknown,
		},
		{
			// The wrapper's own deadline is carried through rather than
			// flattened: a probe that never answered is not a refusal.
			name:    "the completion probe hit the wrapper deadline",
			arrange: func(seams *authTestSeams) { seams.statusErr = context.DeadlineExceeded },
			cause:   authCauseTimeout,
			reason:  authReasonAcceptanceUnknown,
		},
		{
			name:    "the provider refused",
			arrange: func(seams *authTestSeams) { seams.login.refused = true },
			cause:   authCauseProviderRefused,
			reason:  authReasonProviderRefused,
		},
		{
			name:    "the harness exited non-zero",
			arrange: func(seams *authTestSeams) { seams.statusExt = 1 },
			cause:   authCauseProviderRefused,
			reason:  authReasonProviderRefused,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seams := newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			flow := startAuthFlow(t, broker, sessionID)

			testCase.arrange(seams)

			_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
			requireAuthFailed(t, err, testCase.cause)

			status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			require.NoError(t, err)

			reported, ok := status.(authStatusResult)
			require.True(t, ok)
			require.Equal(t, authStateFailed, reported.State)
			require.Equal(t, testCase.reason, reported.Reason)
		})
	}
}

func TestCallbackFailsWhenTheLoginChildIsGone(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	broker.mu.Lock()
	broker.byID[flow.FlowID].login = nil
	broker.mu.Unlock()

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestCallbackFailsWhenTheConfirmationCannotBePersisted(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	original := ledgerMarshal

	ledgerMarshal = func(any) ([]byte, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerMarshal = original })

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestStatusPollsOnlyAfterTheLoginChildExitedAndNoFasterThanTheFloor(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	// authorize read the baseline the completion signal is measured against;
	// every later read is a poll and is counted from here.
	baselineReads := seams.statusCalls

	// A live login child is not a completion signal, and the probe never runs.
	_, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, baselineReads, seams.statusCalls)

	// Within the floor the cached state is served without a native read.
	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, baselineReads, seams.statusCalls)

	// The harness arms a loopback hook unconditionally, so a URL opened on this
	// host completes the login and the child exits without a callback.
	seams.completeLogin()

	seams.login.exited = true

	broker.mu.Lock()
	broker.byID[flow.FlowID].nextProbeAt = time.Time{}
	broker.mu.Unlock()

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, baselineReads+1, seams.statusCalls)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateAuthenticated, reported.State)

	// A terminal flow drives no further native read.
	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, baselineReads+1, seams.statusCalls)
}

func TestStatusProbeLeavesThePendingFlowAloneOnEveryNegativeAnswer(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*authTestSeams, *providerAuth)
	}{
		{
			name:    "the probe failed",
			arrange: func(seams *authTestSeams, _ *providerAuth) { seams.statusErr = errors.New("probe failed") },
		},
		{
			name:    "the account is not logged in",
			arrange: func(seams *authTestSeams, _ *providerAuth) { seams.account = claude.AuthAccount{} },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seams := newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			flow := startAuthFlow(t, broker, sessionID)

			seams.login.exited = true
			testCase.arrange(seams, broker)

			broker.mu.Lock()
			broker.byID[flow.FlowID].nextProbeAt = time.Time{}
			broker.mu.Unlock()

			status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			require.NoError(t, err)

			reported, ok := status.(authStatusResult)
			require.True(t, ok)
			require.Equal(t, authStatePending, reported.State)
		})
	}
}

func TestStatusProbeFailsTheFlowWhenTheConfirmationCannotBePersisted(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	seams.completeLogin()

	seams.login.exited = true

	original := ledgerMarshal

	ledgerMarshal = func(any) ([]byte, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerMarshal = original })

	broker.mu.Lock()
	broker.byID[flow.FlowID].nextProbeAt = time.Time{}
	broker.mu.Unlock()

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateFailed, reported.State)
	require.Equal(t, authReasonAcceptanceUnknown, reported.Reason)
}

func TestStatusRejectsAddressingFailures(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	_, err := broker.status(t.Context(), json.RawMessage(`{"nope":1}`))
	requireInvalidAuthField(t, err, "nope")

	for _, missing := range []string{"sessionId", "providerId", "flowId"} {
		params := flowParams(string(sessionID), flow.FlowID)
		delete(params, missing)

		_, missingErr := broker.status(t.Context(), authParams(t, params))
		requireInvalidAuthField(t, missingErr, missing)
	}

	_, err = broker.status(t.Context(), authParams(t, flowParams("missing", flow.FlowID)))
	requireInvalidAuthField(t, err, "sessionId")

	crossProvider := flowParams(string(sessionID), flow.FlowID)
	crossProvider["providerId"] = "other"

	_, err = broker.status(t.Context(), authParams(t, crossProvider))
	requireInvalidAuthField(t, err, "flowId")
}

func TestCancelIsWrapperOwnedAndIdempotent(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	result, err := broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, result)
	require.Equal(t, 1, seams.login.closeCount())

	again, err := broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, again)
	require.Equal(t, 1, seams.login.closeCount())

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateCancelled, reported.State)
	require.Equal(t, authReasonOwnerCancel, reported.Reason)

	_, err = broker.cancel(t.Context(), json.RawMessage(`{"nope":1}`))
	requireInvalidAuthField(t, err, "nope")
}

// TestTerminalizeKeepsTheFirstTerminalTransition pins the record itself: a flow
// has one terminal transition, and a later one is dropped rather than
// overwriting the owner's.
func TestTerminalizeKeepsTheFirstTerminalTransition(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	presentation := startAuthFlow(t, broker, sessionID)

	flow := broker.byID[presentation.FlowID]
	require.NotNil(t, flow)

	broker.terminalize(flow, authStateCancelled, authReasonOwnerCancel)
	broker.terminalize(flow, authStateAuthenticated, "")

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), presentation.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateCancelled, reported.State)
	require.Equal(t, authReasonOwnerCancel, reported.Reason)
}

// TestSettleAnswersForAFlowClosedUnderIt pins the leg against the record: a
// login child that settled after the owner closed the flow owns no transition
// and writes no confirmation. Reporting the account it left behind would bind a
// resident credential to a connection generation the owner already ended, and
// terminalizing over the closed record would report success for a login the
// owner abandoned.
func TestSettleAnswersForAFlowClosedUnderIt(t *testing.T) {
	cases := []struct {
		name      string
		completed bool
		state     string
		reason    string
		cause     string
	}{
		{
			name:      "cancelled while the login completed",
			completed: true,
			state:     authStateCancelled,
			reason:    authReasonOwnerCancel,
			cause:     authCauseFlowCancelled,
		},
		{
			name:      "expired while the login completed",
			completed: true,
			state:     authStateExpired,
			reason:    authReasonDeadline,
			cause:     authCauseFlowState,
		},
		{
			name:      "cancelled while the provider refused",
			completed: false,
			state:     authStateCancelled,
			reason:    authReasonOwnerCancel,
			cause:     authCauseFlowCancelled,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seams := newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			presentation := startAuthFlow(t, broker, sessionID)

			flow := broker.byID[presentation.FlowID]
			require.NotNil(t, flow)

			if testCase.completed {
				seams.completeLogin()
			}

			broker.terminalize(flow, testCase.state, testCase.reason)

			requireAuthFailed(t, broker.settle(t.Context(), flow), testCase.cause)

			status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), presentation.FlowID)))
			require.NoError(t, err)

			reported, ok := status.(authStatusResult)
			require.True(t, ok)
			require.Equal(t, testCase.state, reported.State)
			require.Equal(t, testCase.reason, reported.Reason)

			record, found, err := broker.ledger.read(authProviderID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, authLedgerIntent, record.State)
		})
	}
}

func TestFlowExpiresOnTheEffectiveDeadline(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	record := broker.byID[flow.FlowID]
	broker.expire(record)

	require.Equal(t, 1, seams.login.closeCount())
	require.Equal(t, authStateExpired, record.state)
	require.Equal(t, authReasonDeadline, record.reason)

	// Expiring a terminal flow changes nothing.
	broker.expire(record)
	require.Equal(t, 1, seams.login.closeCount())
}

func TestCompleterExpiresAnAbandonedFlow(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	original := authNow

	authNow = func() time.Time { return time.Now().Add(-authSafetyDeadline) }

	t.Cleanup(func() { authNow = original })

	result, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	require.NoError(t, err)

	flow, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	require.Eventually(t, func() bool { return seams.login.closeCount() == 1 }, time.Second, 5*time.Millisecond)

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, isStatus := status.(authStatusResult)
	require.True(t, isStatus)
	require.Equal(t, authStateExpired, reported.State)
	require.Equal(t, authReasonDeadline, reported.Reason)
}

// TestSupersedeDropsATerminalFlowWithoutFencingItTwice pins the two halves a
// terminal state splits: the child was already fenced when the flow
// terminalized, so nothing is closed again, but the record is dropped exactly
// as a pending one is.
func TestSupersedeDropsATerminalFlowWithoutFencingItTwice(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	broker.supersede(authFlowKey{sessionID: sessionID, providerID: authProviderID}, authReasonSuperseded)

	flow := startAuthFlow(t, broker, sessionID)
	record := broker.byID[flow.FlowID]

	broker.mu.Lock()
	record.state = authStateFailed
	broker.mu.Unlock()

	broker.supersede(authFlowKey{sessionID: sessionID, providerID: authProviderID}, authReasonSuperseded)
	require.Zero(t, seams.login.closeCount())

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Empty(t, broker.flows)
	require.Empty(t, broker.byID)
	require.Equal(t, authStateFailed, record.state)
}

func TestCloseSessionCancelsEveryPendingFlow(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	(*providerAuth)(nil).closeSession(sessionID)

	flow := startAuthFlow(t, broker, sessionID)
	record := broker.byID[flow.FlowID]

	broker.closeSession("another-session")
	require.Zero(t, seams.login.closeCount())

	broker.closeSession(sessionID)
	require.Equal(t, 1, seams.login.closeCount())

	broker.mu.Lock()
	require.Equal(t, authStateCancelled, record.state)
	require.Equal(t, authReasonSessionClosed, record.reason)
	broker.mu.Unlock()

	// The session that owned the flow is gone, so the id it minted addresses
	// nothing rather than answering out of a record nothing can free.
	_, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)
}

func TestCloseSessionSkipsAlreadyTerminalFlows(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)

	broker.mu.Lock()
	broker.byID[flow.FlowID].state = authStateFailed
	broker.mu.Unlock()

	broker.closeSession(sessionID)
	require.Zero(t, seams.login.closeCount())
}

func TestAwaitLoginExitReturnsOnADeadline(t *testing.T) {
	newAuthSeams(t)

	broker, _ := newAuthBroker(t)
	blocked := make(chan struct{})

	handle := &authLoginHandle{
		agent:   broker.agent,
		release: func() {},
		login:   &blockingAuthLogin{release: blocked},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	broker.awaitLoginExit(ctx, handle)
	close(blocked)
}

// blockingAuthLogin never completes its close until the test releases it.
type blockingAuthLogin struct {
	release chan struct{}
}

func (*blockingAuthLogin) Submit(string) error { return nil }
func (*blockingAuthLogin) Exited() bool        { return false }

func (l *blockingAuthLogin) Close() error {
	<-l.release

	return nil
}

// The flow is addressable before its child exists, so a leg can close it while
// the mint is still running. That leg fences a handle the mint has not
// published, so the mint has to fence the one it is holding.
func TestAuthorizeFencesALoginChildTheFlowClosedUnder(t *testing.T) {
	seams := newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	begin := authLoginBegin
	authLoginBegin = func(
		ctx context.Context,
		options claude.Options,
		flowGeneration *claude.DarwinGeneration,
	) (authLoginSession, string, error) {
		broker.closeSession(sessionID)

		return begin(ctx, options, flowGeneration)
	}

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseFlowCancelled)

	require.Equal(t, 1, seams.login.closeCount())
}

func TestDecodeAuthorizeRequestRejectsEveryMissingField(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	for _, missing := range []string{
		"sessionId", "providerId", "connectionId",
		"methodsGeneration", authFieldMethod, "authorizeRequestId",
	} {
		params := authorizeParams(sessionID, generation)
		delete(params, missing)

		_, err := broker.authorize(t.Context(), authParams(t, params))
		requireInvalidAuthField(t, err, missing)
	}
}

func TestAuthorizeFailsWhenTheIntentCannotBePersisted(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	original := ledgerRename

	ledgerRename = func(string, string) error { return errTestRandom }

	t.Cleanup(func() { ledgerRename = original })

	_, err := broker.authorize(t.Context(), authParams(t, authorizeParams(sessionID, generation)))
	requireAuthFailed(t, err, authCauseProcess)
}
