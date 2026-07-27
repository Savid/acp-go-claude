package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

	// The superseded flow stays addressable and reports its terminal state.
	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), first.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateCancelled, reported.State)
	require.Equal(t, authReasonSuperseded, reported.Reason)
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
			cause:   authCauseTransport,
			reason:  authReasonAcceptanceUnknown,
		},
		{
			name:    "the provider refused",
			arrange: func(seams *authTestSeams) { seams.account = claude.AuthAccount{} },
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

	// A live login child is not a completion signal, and the probe never runs.
	_, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Zero(t, seams.statusCalls)

	// Within the floor the cached state is served without a native read.
	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Zero(t, seams.statusCalls)

	seams.login.exited = true

	broker.mu.Lock()
	broker.byID[flow.FlowID].nextProbeAt = time.Time{}
	broker.mu.Unlock()

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, 1, seams.statusCalls)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateAuthenticated, reported.State)

	// A terminal flow drives no further native read.
	_, err = broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)
	require.Equal(t, 1, seams.statusCalls)
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

func TestSupersedeIgnoresAbsentAndTerminalFlows(t *testing.T) {
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
}

func TestCloseSessionCancelsEveryPendingFlow(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	(*providerAuth)(nil).closeSession(sessionID)

	flow := startAuthFlow(t, broker, sessionID)

	broker.closeSession("another-session")
	require.Zero(t, seams.login.closeCount())

	broker.closeSession(sessionID)
	require.Equal(t, 1, seams.login.closeCount())

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateCancelled, reported.State)
	require.Equal(t, authReasonSessionClosed, reported.Reason)
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
