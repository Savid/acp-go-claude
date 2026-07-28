package claudeacp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

// authAdmissionWait bounds a wait for a second leg to reach a barrier. A
// discipline that admits only one never sends it, so the wait has to end by
// itself rather than block.
const authAdmissionWait = 300 * time.Millisecond

// countAuthLogins reports how many login children the surface started.
func countAuthLogins(t *testing.T) *atomic.Int64 {
	t.Helper()

	starts := &atomic.Int64{}
	original := authLoginBegin

	t.Cleanup(func() { authLoginBegin = original })

	authLoginBegin = func(
		ctx context.Context,
		options claude.Options,
		generation *claude.DarwinGeneration,
	) (authLoginSession, string, error) {
		starts.Add(1)

		return original(ctx, options, generation)
	}

	return starts
}

// authBarrier parks the leg that reaches the swapped seam and releases every
// parked leg together.
type authBarrier struct {
	arrived chan struct{}
	release chan struct{}
}

func newAuthBarrier() *authBarrier {
	return &authBarrier{arrived: make(chan struct{}, 4), release: make(chan struct{})}
}

func (b *authBarrier) park() {
	b.arrived <- struct{}{}
	<-b.release
}

func (b *authBarrier) awaitSecond() {
	select {
	case <-b.arrived:
	case <-time.After(authAdmissionWait):
	}
}

// holdAuthSlot takes the credential-slot gate so a test can park a leg on it and
// hand that leg a context that has already ended, which is the one way a leg
// leaves the queue without the gate.
func holdAuthSlot(t *testing.T, broker *providerAuth) func() {
	t.Helper()

	release, admitted := broker.admitSlot(t.Context())
	require.True(t, admitted)
	t.Cleanup(release)

	return release
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	return ctx
}

// TestSessionCloseRefusesAnAuthorizeThatHasNotPublished races a close against an
// authorize that already passed its session lookup. Publication is the
// authoritative check because the sweep set close takes is only complete if
// nothing can join it afterwards: a flow published after that set was taken
// starts a login child in the config dir with nothing left that can ever fence
// it.
func TestSessionCloseRefusesAnAuthorizeThatHasNotPublished(t *testing.T) {
	newAuthSeams(t)

	starts := countAuthLogins(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	barrier := newAuthBarrier()
	original := ledgerMarshal

	t.Cleanup(func() { ledgerMarshal = original })

	ledgerMarshal = func(value any) ([]byte, error) {
		barrier.park()

		return original(value)
	}

	answered := make(chan error, 1)

	go func() {
		_, err := broker.authorize(context.Background(), authParams(t, authorizeParams(sessionID, generation)))
		answered <- err
	}()

	<-barrier.arrived

	broker.closeSession(sessionID)
	close(barrier.release)

	requireInvalidAuthField(t, <-answered, acpFieldSessionID)
	require.Zero(t, starts.Load())

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Empty(t, broker.flows)
	require.Empty(t, broker.byID)
}

// TestALateLegIsRefusedOnAClosedSession pins the cheap half. session/close
// terminalizes the flows before it drops the id from the live session map, so
// between those two the id still resolves to a session that can no longer own a
// login.
func TestALateLegIsRefusedOnAClosedSession(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	broker.closeSession(sessionID)

	_, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	requireInvalidAuthField(t, err, acpFieldSessionID)
}

// TestAReopenedSessionIdServesTheAuthSurfaceAgain pins the difference between
// closing an id and deleting one. session/close drops an id from the live map
// without tombstoning it, and load, resume, and fork all republish a
// caller-supplied one, so a close mark left behind would refuse every
// provider-auth leg of a session the host can see and prompt through. Only
// session/delete keeps an id refused for good, and it tombstones the id itself.
func TestAReopenedSessionIdServesTheAuthSurfaceAgain(t *testing.T) {
	newAuthSeams(t)

	broker, sessionID := newAuthBroker(t)
	agent := broker.agent

	broker.closeSession(sessionID)

	agent.mu.Lock()
	delete(agent.sessions, sessionID)
	agent.mu.Unlock()

	require.NoError(t, agent.storeStartedSession(t.Context(), &agentSession{agent: agent, id: sessionID}))

	flow := startAuthFlow(t, broker, sessionID)
	require.NotEmpty(t, flow.FlowID)
	require.False(t, broker.sessionClosed(sessionID))
}

// TestARetiredAuthorizeRequestIdCannotCancelItsSuccessor delivers a delayed
// transport retry of a request a later authorize already replaced. Only the
// newest record is answerable verbatim, so an older key is unanswerable — and
// minting in its place destroys the live flow whose URL the operator is looking
// at, which is the one thing an idempotency key exists to prevent.
func TestARetiredAuthorizeRequestIdCannotCancelItsSuccessor(t *testing.T) {
	newAuthSeams(t)

	starts := countAuthLogins(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	authorize := func(requestID string) (any, error) {
		params := authorizeParams(sessionID, generation)
		params["authorizeRequestId"] = requestID

		return broker.authorize(t.Context(), authParams(t, params))
	}

	_, err := authorize("request-a")
	require.NoError(t, err)

	result, err := authorize("request-b")
	require.NoError(t, err)

	second, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	_, err = authorize("request-c")
	require.NoError(t, err)

	_, err = authorize("request-a")
	requireInvalidAuthField(t, err, authFieldAuthorizeRequestID)

	_, err = authorize("request-b")
	requireInvalidAuthField(t, err, authFieldAuthorizeRequestID)

	// The successor the retry would have torn down is still the live flow, and
	// the retry started no login of its own.
	require.Equal(t, int64(3), starts.Load())

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), second.FlowID)))
	requireInvalidAuthField(t, err, authFieldFlowID)
	require.Nil(t, status)
}

// TestConcurrentIdenticalAuthorizesMintOneLogin runs two identical requests that
// arrive before either has published. Unadmitted they never see each other — the
// idempotency record only exists after the mint — and both start a `claude auth
// login` child, after which only the second is addressable and the first is a
// live child holding a native root nothing can reclaim.
func TestConcurrentIdenticalAuthorizesMintOneLogin(t *testing.T) {
	newAuthSeams(t)

	starts := countAuthLogins(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)

	barrier := newAuthBarrier()
	original := authRandRead

	t.Cleanup(func() { authRandRead = original })

	authRandRead = func(value []byte) (int, error) {
		barrier.park()

		return original(value)
	}

	type outcome struct {
		presentation authAuthorizeResult
		err          error
	}

	answered := make(chan outcome, 2)

	call := func() {
		result, err := broker.authorize(context.Background(), authParams(t, authorizeParams(sessionID, generation)))
		presentation, _ := result.(authAuthorizeResult)
		answered <- outcome{presentation: presentation, err: err}
	}

	go call()

	<-barrier.arrived

	// The repeat arrives while the first request is still short of publishing.
	go call()
	barrier.awaitSecond()
	close(barrier.release)

	first, second := <-answered, <-answered
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Equal(t, first.presentation, second.presentation)
	require.Equal(t, int64(1), starts.Load())

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Len(t, broker.byID, 1)
}

// TestOneCallbackIsAdmittedForOneLoginChild runs two callbacks that both address
// the same live flow. Unclaimed, both pass a terminal check neither has set yet
// and both write to and then close one child's stdin: the second paste lands in
// a pipe already closed behind the first, and whichever value the harness
// happened to read decides an answer the owner never chose. Nothing here is a
// data race — every field access is mutex-guarded — so a green -race run says
// nothing about it.
func TestOneCallbackIsAdmittedForOneLoginChild(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	barrier := newAuthBarrier()
	seams.login.beforeSubmit = barrier.park

	params := authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue))
	answered := make(chan error, 2)

	call := func() {
		_, err := broker.callback(context.Background(), params)
		answered <- err
	}

	go call()

	<-barrier.arrived

	go call()
	barrier.awaitSecond()
	close(barrier.release)

	first, second := <-answered, <-answered

	admitted := 0

	for _, err := range []error{first, second} {
		if err == nil {
			admitted++

			continue
		}

		requireAuthFailed(t, err, authCauseFlowState)
	}

	require.Equal(t, 1, admitted)
	require.Len(t, seams.login.values(), 1)
}

// TestTheStatusProbeSkipsAFlowAnotherLegIsDriving pins the poll's half of the
// claim. Whoever holds it is already driving this completion, and a consumer's
// poll cadence must never end up queued behind a native call it has nothing to
// add to.
func TestTheStatusProbeSkipsAFlowAnotherLegIsDriving(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	flow := startAuthFlow(t, broker, sessionID)
	seams.login.markExited()

	record := broker.byID[flow.FlowID]
	require.NoError(t, broker.claimFlow(record))

	result, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	require.NoError(t, err)

	reported, ok := result.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStatePending, reported.State)
	requireAuthFailed(t, broker.claimFlow(record), authCauseFlowState)

	broker.releaseFlow(record)
	require.NoError(t, broker.claimFlow(record))
}

// TestDisconnectFencesTheLoginsItsAbsenceProofDependsOn pins what an absence
// proof is worth while a login child is still running. The harness arms its
// loopback hook unconditionally, so a browser tab reaching it installs a
// credential with no leg driving it: without the fence the removal proves the
// slot empty, records it removed, and the child fills it again a moment later,
// leaving a live credential under a record that says there is none.
func TestDisconnectFencesTheLoginsItsAbsenceProofDependsOn(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newDisconnectBroker(t)

	terminal := startAuthFlow(t, broker, sessionID)

	_, err := broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), terminal.FlowID, testPastedValue)))
	require.NoError(t, err)

	params := authorizeParams(sessionID, broker.generation)
	params["authorizeRequestId"] = "request-2"

	result, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)

	pending, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	seams.account = claude.AuthAccount{}
	closes := seams.login.closeCount()

	_, err = broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
	require.NoError(t, err)
	require.Equal(t, closes+1, seams.login.closeCount())

	status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), pending.FlowID)))
	require.NoError(t, err)

	reported, ok := status.(authStatusResult)
	require.True(t, ok)
	require.Equal(t, authStateCancelled, reported.State)
	require.Equal(t, authReasonOwnerCancel, reported.Reason)
}

// TestDisconnectIsNotUndoneByAnAdmittedCompletion parks a paste that has already
// passed every check the broker makes and lets the owner disconnect underneath
// it. The completion then arrives holding a reading and a binding the removal
// replaced: confirming on it would write the retired generation back over the
// bump and tell the host a disconnect it can no longer repeat succeeded, with
// the account still reachable.
func TestDisconnectIsNotUndoneByAnAdmittedCompletion(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newDisconnectBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	barrier := newAuthBarrier()
	seams.login.beforeSubmit = barrier.park

	answered := make(chan error, 1)

	go func() {
		_, err := broker.callback(context.Background(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
		answered <- err
	}()

	<-barrier.arrived

	seams.account = claude.AuthAccount{}

	_, err := broker.disconnect(t.Context(), authParams(t, disconnectParams(sessionID, 1)))
	require.NoError(t, err)

	close(barrier.release)
	requireAuthFailed(t, <-answered, authCauseFlowCancelled)

	record, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerRemoved, record.State)
	require.Equal(t, int64(2), record.BindingGeneration)
}

// TestACompletionRefusesALineageThatMovedUnderIt pins the completion's own
// check, which is what answers a disconnect that already finished rather than
// one still running. The credential the child installed is resident either way;
// what the check refuses is claiming a binding the record no longer names.
func TestACompletionRefusesALineageThatMovedUnderIt(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)

	flow := startAuthFlow(t, broker, sessionID)

	record, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)

	record.BindingGeneration++
	require.NoError(t, broker.ledger.write(record))

	_, err = broker.callback(t.Context(), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
	requireAuthFailed(t, err, authCauseBindingConflict)
	require.True(t, seams.account.LoggedIn)

	after, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerIntent, after.State)
}

// TestAGateOutlivesEveryWaiterButItsLast pins the gate's lifetime to its
// refcount. A gate removed while a leg still holds it is replaced by a fresh one
// the next leg walks straight through, so the serialization silently stops
// happening while every map operation still looks correct; a gate nobody holds
// or wants orders nothing and has to go, because the keys are unbounded over the
// life of an agent that outlives its sessions.
func TestAGateOutlivesEveryWaiterButItsLast(t *testing.T) {
	broker, sessionID := newAuthBroker(t)
	key := authFlowKey{sessionID: sessionID, providerID: authProviderID}

	release, admitted := broker.admitKey(t.Context(), key)
	require.True(t, admitted)

	broker.mu.Lock()
	held := broker.admissions[key]
	broker.mu.Unlock()

	require.NotNil(t, held)

	// A leg that stopped waiting takes nothing and so releases nothing, and the
	// gate it left survives for the holder that is still inside it.
	_, admitted = broker.admitKey(cancelledContext(t), key)
	require.False(t, admitted)

	broker.mu.Lock()
	require.Same(t, held, broker.admissions[key])
	broker.mu.Unlock()

	release()

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Empty(t, broker.admissions)
}

// TestAdmissionRefusesEveryLegTheCallerAbandoned walks each gated leg with a
// context that has already ended while the gate it needs is held elsewhere. A
// leg that leaves the queue without the gate has no right to mutate what the
// gate names, so it answers rather than proceeding.
func TestAdmissionRefusesEveryLegTheCallerAbandoned(t *testing.T) {
	t.Run("authorize", func(t *testing.T) {
		newAuthSeams(t)

		broker, sessionID := newAuthBroker(t)
		generation := authCatalogGeneration(t, broker, sessionID)
		key := authFlowKey{sessionID: sessionID, providerID: authProviderID}

		release, admitted := broker.admitKey(t.Context(), key)
		require.True(t, admitted)

		defer release()

		_, err := broker.authorize(cancelledContext(t), authParams(t, authorizeParams(sessionID, generation)))
		requireAuthFailed(t, err, authCauseTimeout)
	})

	t.Run("intent", func(t *testing.T) {
		newAuthSeams(t)

		broker, sessionID := newAuthBroker(t)
		generation := authCatalogGeneration(t, broker, sessionID)

		holdAuthSlot(t, broker)

		ctx, cancel := context.WithCancel(context.Background())
		barrier := newAuthBarrier()
		original := authRandRead

		t.Cleanup(func() { authRandRead = original })

		authRandRead = func(value []byte) (int, error) {
			barrier.park()

			return original(value)
		}

		answered := make(chan error, 1)

		go func() {
			_, err := broker.authorize(ctx, authParams(t, authorizeParams(sessionID, generation)))
			answered <- err
		}()

		<-barrier.arrived
		cancel()
		close(barrier.release)

		requireAuthFailed(t, <-answered, authCauseTimeout)
	})

	t.Run("callback", func(t *testing.T) {
		newAuthSeams(t)

		broker, sessionID := newAuthBroker(t)
		flow := startAuthFlow(t, broker, sessionID)

		holdAuthSlot(t, broker)

		_, err := broker.callback(cancelledContext(t), authParams(t, callbackParams(string(sessionID), flow.FlowID, testPastedValue)))
		requireAuthFailed(t, err, authCauseTimeout)
	})

	t.Run("status", func(t *testing.T) {
		seams := newAuthSeams(t)

		broker, sessionID := newAuthBroker(t)
		flow := startAuthFlow(t, broker, sessionID)
		seams.login.markExited()

		holdAuthSlot(t, broker)

		result, err := broker.status(cancelledContext(t), authParams(t, flowParams(string(sessionID), flow.FlowID)))
		require.NoError(t, err)

		reported, ok := result.(authStatusResult)
		require.True(t, ok)
		require.Equal(t, authStatePending, reported.State)
	})

	t.Run("disconnect", func(t *testing.T) {
		seams := newAuthSeams(t)

		broker, sessionID := newDisconnectBroker(t)
		startAuthFlow(t, broker, sessionID)

		holdAuthSlot(t, broker)

		_, err := broker.disconnect(cancelledContext(t), authParams(t, disconnectParams(sessionID, 1)))
		requireAuthFailed(t, err, authCauseTimeout)
		require.Zero(t, seams.logoutCalls)
	})
}
