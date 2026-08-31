package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func newLifecycleStreamTestSession(t *testing.T) (*agentSession, *recordingAgentClient, *sessionStream) {
	t.Helper()
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	session := &agentSession{agent: agent, id: "session-1"}
	stream := session.lifecycleStream()
	require.NotNil(t, stream)

	return session, conn, stream
}

// closeCommit is the durable rung a close settlement runs between its terminal
// transitions and its quiescence fact. Cases that drive the stream directly own
// that rung here, so a test can land it or refuse it without a store behind the
// session.
func closeCommit(err error) func() error {
	return func() error { return err }
}

// writePreparedActionForTest crosses the same boundary as the production ACP
// writer: reserve the action, then bind it to a concrete method and request id
// only after the request has been written.
func writePreparedActionForTest(
	ctx context.Context,
	stream *sessionStream,
	route string,
	kind lifecycle.ActionKind,
) (lifecycle.ActionUpdate, error) {
	update, err := stream.prepareAction(ctx, route, kind)
	if err != nil || update.ActionID == "" {
		return update, err
	}

	method := acp.ClientMethodSessionRequestPermission
	if kind == lifecycle.ActionElicitation {
		method = acp.ClientMethodElicitationCreate
	}
	err = stream.announcePreparedAction(ctx, update, actionWireIdentity{
		method:    method,
		requestID: "test-wire-" + update.ActionID,
	})

	return update, err
}

// lifecycleEventTypes reports, in delivery order, the event type of every
// lifecycle-bearing notification the host received.
func lifecycleEventTypes(t *testing.T, conn *recordingAgentClient) []string {
	t.Helper()

	types := []string{}

	for _, update := range conn.Updates() {
		envelope, ok := update.Meta[lifecycle.MetaKey].(map[string]any)
		if !ok {
			continue
		}

		eventType, ok := requireAnyMap(t, envelope["event"])["type"].(string)
		require.True(t, ok, "every envelope names its event type")

		types = append(types, eventType)
	}

	return types
}

// newAuthoritativeLifecycleStreamTestSession builds the stream on a
// configuration whose containment boundary proves an authoritative quiescence
// source, so the settlement fact is one this session could actually state. A
// test that asserts no fact was stated is only a test on this fixture: on a
// session that negotiated no authoritative quiescence, no fact could ever emit
// and the assertion could never fail.
func newAuthoritativeLifecycleStreamTestSession(t *testing.T) (*agentSession, *recordingAgentClient, *sessionStream) {
	t.Helper()

	agent := NewAgent(WithHostAuthority(newFakeHostAuthority()))
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.True(t, agent.negotiatedLifecycle().AuthoritativeQuiescence)

	session := &agentSession{agent: agent, id: "session-1"}
	stream := session.lifecycleStream()
	require.NotNil(t, stream)

	return session, conn, stream
}

func newLifecycleActionSession(t *testing.T, nonce string) (*agentSession, *recordingAgentClient, *sessionStream, context.Context) {
	t.Helper()
	session, conn, stream := newLifecycleStreamTestSession(t)
	ctx := t.Context()
	require.NoError(t, stream.incarnate(ctx))
	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, nonce, func() error { return nil })
	require.NoError(t, err)

	admission := &controlCallbackAdmission{session: session, route: nonce}
	session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{admission: {}}
	t.Cleanup(func() {
		session.callbackOwnershipMu.Lock()
		delete(session.callbackAdmissions, admission)
		session.callbackOwnershipMu.Unlock()
	})

	return session, conn, stream, context.WithValue(ctx, controlCallbackAdmissionContextKey{}, admission)
}

func TestSessionStreamOwnershipEdgeStates(t *testing.T) {
	ctx := t.Context()

	var absent *sessionStream
	require.NoError(t, absent.announcePreparedAction(ctx, lifecycle.ActionUpdate{}, actionWireIdentity{}))
	require.False(t, actionMethodMatches(lifecycle.ActionKind("unknown"), "unknown"))
	turnID, err := absent.openAgentTurn(ctx, "route")
	require.NoError(t, err)
	require.Empty(t, turnID)
	_, err = absent.emitCallbackContent(ctx, "route", func() error { return nil })
	require.NoError(t, err)

	session, _, stream := newLifecycleStreamTestSession(t)
	turnID, err = stream.openAgentTurn(ctx, "route")
	require.NoError(t, err)
	require.Empty(t, turnID)
	_, live := stream.callbackOwner("route")
	require.False(t, live)

	require.NoError(t, stream.incarnate(ctx))
	session.setAutonomousRoute("route", nil)
	stream.lost = errors.New("stream lost")
	_, err = stream.openAgentTurn(ctx, "route")
	require.ErrorContains(t, err, "stream lost")
	stream.lost = nil

	turnID, err = stream.openAgentTurn(ctx, "route")
	require.NoError(t, err)
	require.NotEmpty(t, turnID)
	_, err = stream.dispatch(ctx, lifecycle.Submission{}, "prompt", func() error { return nil })
	require.Error(t, err)
	again, err := stream.openAgentTurn(ctx, "route")
	require.NoError(t, err)
	require.Empty(t, again)

	_, live = stream.callbackOwner("wrong-route")
	require.False(t, live)
	_, err = writePreparedActionForTest(ctx, stream, "wrong-route", lifecycle.ActionPermission)
	require.NoError(t, err)
	prepared, err := stream.prepareAction(ctx, "route", lifecycle.ActionPermission)
	require.NoError(t, err)

	stream.lost = errors.New("turn stream lost")
	err = stream.announcePreparedAction(ctx, prepared, actionWireIdentity{
		method: acp.ClientMethodSessionRequestPermission, requestID: "lost",
	})
	require.ErrorContains(t, err, "turn stream lost")
	_, err = writePreparedActionForTest(ctx, stream, "route", lifecycle.ActionPermission)
	require.ErrorContains(t, err, "turn stream lost")
	_, err = stream.emitCallbackContent(ctx, "route", func() error { return nil })
	require.ErrorContains(t, err, "turn stream lost")
	stream.lost = nil

	_, err = stream.emitCallbackContent(ctx, "wrong-route", func() error { return nil })
	require.Error(t, err)
	emitErr := errors.New("callback content failed")
	_, err = stream.emitCallbackContent(ctx, "route", func() error { return emitErr })
	require.ErrorIs(t, err, emitErr)
}

func TestSessionStreamAutonomousCallbackOpeningEdges(t *testing.T) {
	t.Run("action route retired", func(t *testing.T) {
		session, _, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(t.Context()))
		session.setAutonomousRoute("live-route", &nativeIncarnation{})

		update, err := writePreparedActionForTest(t.Context(), stream, "retired-route", lifecycle.ActionPermission)
		require.NoError(t, err)
		require.Empty(t, update.ActionID)
	})

	t.Run("action turn opening fails", func(t *testing.T) {
		session, conn, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(t.Context()))
		session.setAutonomousRoute("live-route", &nativeIncarnation{})
		conn.sessionUpdateErr = errors.New("agent turn delivery failed")

		_, err := writePreparedActionForTest(t.Context(), stream, "live-route", lifecycle.ActionPermission)
		require.ErrorContains(t, err, "lifecycle delivery failed")
	})

	t.Run("content route retired", func(t *testing.T) {
		session, _, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(t.Context()))
		session.setAutonomousRoute("live-route", &nativeIncarnation{})

		_, err := stream.emitCallbackContent(t.Context(), "retired-route", func() error { return nil })
		require.ErrorContains(t, err, "no longer has a live owner")
	})

	t.Run("content turn opening fails", func(t *testing.T) {
		session, conn, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(t.Context()))
		incarnation := &nativeIncarnation{}
		session.setAutonomousRoute("live-route", incarnation)
		conn.sessionUpdateErr = errors.New("agent turn delivery failed")

		owner, err := stream.emitCallbackContent(t.Context(), "live-route", func() error { return nil })
		require.Same(t, incarnation, owner)
		require.ErrorContains(t, err, "lifecycle delivery failed")
	})
}

func TestSessionStreamActionContentEdgeStates(t *testing.T) {
	ctx := t.Context()
	session, conn, stream, turnCtx := newLifecycleActionSession(t, "action-route")
	defer session.stopNativePump()

	action, err := session.beginLifecycleAction(turnCtx, lifecycle.ActionPermission)
	require.NoError(t, err)
	var absentAction *sessionLifecycleAction
	require.Error(t, absentAction.afterWireWrite(ctx, actionWireIdentity{}))
	require.False(t, absentAction.exactOwnerCurrentLocked())
	require.False(t, absentAction.responseOwnerCurrent())
	absentAction.failOwner(ctx, errors.New("ignored"), "test")
	require.NoError(t, absentAction.resolve(ctx, lifecycle.ActionFailed))
	missingRegistration := &sessionLifecycleAction{session: session, admission: &controlCallbackAdmission{session: session, route: "missing"}}
	require.False(t, missingRegistration.exactOwnerCurrentLocked())

	unannounced, err := session.beginLifecycleAction(turnCtx, lifecycle.ActionElicitation)
	require.NoError(t, err)
	require.Error(t, unannounced.resolve(ctx, lifecycle.ActionFailed))
	containmentCtx, contain := context.WithCancelCause(ctx)
	contain(errExactInteractionContainment)
	require.Equal(t, lifecycle.ActionFailed, interactionActionState(containmentCtx, lifecycle.ActionCancelled))

	missingUpdate := action.update
	missingUpdate.Owner.ID = "missing-owner"
	err = stream.announcePreparedAction(ctx, missingUpdate, actionWireIdentity{
		method:    acp.ClientMethodSessionRequestPermission,
		requestID: "missing-wire",
	})
	require.Error(t, err)

	err = action.wireAdmission(turnCtx, nil).observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodElicitationCreate,
		requestID: "wrong-family",
	})
	require.ErrorContains(t, err, "no exact host request write")
	require.False(t, action.announced)
	err = action.wireAdmission(turnCtx, func() error {
		return errors.New("content failed")
	}).publishPending()
	require.ErrorContains(t, err, "content failed")
	require.False(t, action.announced)

	staleAction, err := session.beginLifecycleAction(turnCtx, lifecycle.ActionPermission)
	require.NoError(t, err)
	admission := controlCallbackAdmissionFromContext(turnCtx)
	session.callbackOwnershipMu.Lock()
	delete(session.callbackAdmissions, admission)
	session.callbackOwnershipMu.Unlock()
	require.Error(t, staleAction.afterWireWrite(ctx, actionWireIdentity{
		method: acp.ClientMethodSessionRequestPermission, requestID: "stale-owner",
	}))
	session.callbackOwnershipMu.Lock()
	session.callbackAdmissions[admission] = struct{}{}
	session.callbackOwnershipMu.Unlock()

	session.closing = true
	err = action.wireAdmission(turnCtx, nil).observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodSessionRequestPermission,
		requestID: "closing",
	})
	require.NoError(t, err, "a fully written admitted callback announces across the close latch")
	require.True(t, action.announced)
	require.NoError(t, action.resolve(ctx, lifecycle.ActionAccepted),
		"close owns the terminal resolution once its latch is visible")
	err = stream.announcePreparedAction(ctx, action.update, actionWireIdentity{
		method: acp.ClientMethodSessionRequestPermission, requestID: "duplicate",
	})
	require.Error(t, err)
	require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))

	terminal := 0
	for _, notification := range conn.Updates() {
		envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any)
		if !ok {
			continue
		}
		event := requireAnyMap(t, envelope["event"])
		if event["type"] != string(lifecycle.EventActionUpdate) {
			continue
		}
		update := requireAnyMap(t, event["action"])
		if update["actionId"] == action.update.ActionID && update["state"] == string(lifecycle.ActionCancelled) {
			terminal++
		}
	}
	require.Equal(t, 1, terminal, "the exact fully written action terminalizes once before the fence")
}

func TestLifecycleActionFailuresContainTheirExactNativeOwner(t *testing.T) {
	newAction := func(t *testing.T) (*agentSession, *recordingAgentClient, *nativeIncarnation, *controlCallbackAdmission, *sessionLifecycleAction) {
		t.Helper()
		session, _, conn, cleanup := newNegotiatedPromptFlowSession(t)
		t.Cleanup(cleanup)
		require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
		incarnation := session.currentNativeIncarnation()
		stream := session.lifecycleStream()
		_, err := stream.dispatch(t.Context(), lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "action-turn", func() error { return nil })
		require.NoError(t, err)
		admission := &controlCallbackAdmission{session: session, route: "action-turn", incarnation: incarnation}
		session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{admission: {}}
		turnCtx := context.WithValue(t.Context(), controlCallbackAdmissionContextKey{}, admission)
		action, err := session.beginLifecycleAction(turnCtx, lifecycle.ActionPermission)
		require.NoError(t, err)
		require.NoError(t, action.afterWireWrite(t.Context(), actionWireIdentity{
			method: acp.ClientMethodSessionRequestPermission, requestID: "native-owner",
		}))
		incarnation.superviseOnce.Do(func() {})

		return session, conn, incarnation, admission, action
	}

	t.Run("owner retired before resolution", func(t *testing.T) {
		session, _, incarnation, admission, action := newAction(t)
		session.callbackOwnershipMu.Lock()
		delete(session.callbackAdmissions, admission)
		session.callbackOwnershipMu.Unlock()
		require.Error(t, action.resolve(t.Context(), lifecycle.ActionAccepted))
		require.True(t, incarnation.failed.Load())
	})

	t.Run("resolution projection fails", func(t *testing.T) {
		session, conn, incarnation, _, action := newAction(t)
		session.agent.setConnection(&lifecycleEventFailingClient{
			recordingAgentClient: conn,
			eventType:            lifecycle.EventActionUpdate,
			err:                  errors.New("resolution projection failed"),
		})
		require.Error(t, action.resolve(t.Context(), lifecycle.ActionAccepted))
		require.True(t, incarnation.failed.Load())
	})
}

func TestBeginLifecycleActionRejectsTerminalOwners(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	_, err := session.beginLifecycleAction(t.Context(), lifecycle.ActionPermission)
	require.Error(t, err)

	_, err = session.beginLifecycleAction(withTurnRoute(t.Context(), "stale"), lifecycle.ActionPermission)
	require.Error(t, err)
	staleAdmission := &controlCallbackAdmission{session: session, route: "stale"}
	staleCtx := context.WithValue(t.Context(), controlCallbackAdmissionContextKey{}, staleAdmission)
	_, err = session.beginLifecycleAction(staleCtx, lifecycle.ActionPermission)
	require.Error(t, err)

	closingAdmission := &controlCallbackAdmission{session: session, route: "closing"}
	session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{closingAdmission: {}}
	closingCtx := context.WithValue(t.Context(), controlCallbackAdmissionContextKey{}, closingAdmission)
	session.closing = true
	_, err = session.beginLifecycleAction(closingCtx, lifecycle.ActionPermission)
	require.Error(t, err)
	session.closing = false

	failed := &nativeIncarnation{}
	failed.failed.Store(true)
	failed.superviseOnce.Do(func() {})
	failedAdmission := &controlCallbackAdmission{session: session, route: "failed", incarnation: failed}
	session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{failedAdmission: {}}
	failedCtx := context.WithValue(t.Context(), controlCallbackAdmissionContextKey{}, failedAdmission)
	_, err = session.beginLifecycleAction(failedCtx, lifecycle.ActionPermission)
	require.Error(t, err)

	current := &nativeIncarnation{}
	current.superviseOnce.Do(func() {})
	currentAdmission := &controlCallbackAdmission{session: session, route: "old", incarnation: current}
	session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{currentAdmission: {}}
	currentCtx := context.WithValue(t.Context(), controlCallbackAdmissionContextKey{}, currentAdmission)
	_, err = session.beginLifecycleAction(currentCtx, lifecycle.ActionPermission)
	require.Error(t, err)
	session.stopNativePump()
}

type lifecycleActionFailingClient struct {
	*recordingAgentClient
	state  lifecycle.ActionState
	err    error
	failed chan lifecycle.ActionState
}

func (c *lifecycleActionFailingClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any)
	if ok {
		event, eventOK := envelope["event"].(map[string]any)
		action, actionOK := event["action"].(map[string]any)
		if eventOK && actionOK && event["type"] == string(lifecycle.EventActionUpdate) && action["state"] == string(c.state) {
			if c.failed != nil {
				c.failed <- c.state
			}

			return c.err
		}
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

func failLifecycleAction(
	session *agentSession,
	conn *recordingAgentClient,
	state lifecycle.ActionState,
	err error,
) {
	session.agent.setConnection(&lifecycleActionFailingClient{
		recordingAgentClient: conn,
		state:                state,
		err:                  err,
	})
}

// TestSessionStreamRoutesActionsAndSettlesOnce exercises the public lifecycle
// story as one reducible stream: acceptance, two blocking actions, release of
// only the final blocker, and a terminal turn settlement that is idempotent.
func TestSessionStreamRoutesActionsAndSettlesOnce(t *testing.T) {
	ctx := t.Context()
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))

	sent := 0
	turnID, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "submission-1", ClientNonce: "client-1", RunID: "run-1"}, "nonce-1", func() error {
		sent++

		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, turnID)
	require.Equal(t, 1, sent)

	first, err := writePreparedActionForTest(ctx, stream, "nonce-1", lifecycle.ActionPermission)
	require.NoError(t, err)
	second, err := writePreparedActionForTest(ctx, stream, "nonce-1", lifecycle.ActionElicitation)
	require.NoError(t, err)
	require.NotEqual(t, first.ActionID, second.ActionID)
	require.NotNil(t, first.BlocksForeground)
	require.True(t, *first.BlocksForeground, "first sight must explicitly state blocksForeground")
	require.Equal(t, "run-1", first.RunID)

	before := len(conn.Updates())
	require.NoError(t, stream.resolveAction(ctx, first, lifecycle.ActionAccepted))
	require.Len(t, conn.Updates(), before+1, "one remaining blocker must keep requires_action latched")
	require.NoError(t, stream.resolveAction(ctx, second, lifecycle.ActionDeclined))
	require.NoError(t, stream.resolveAction(ctx, second, lifecycle.ActionAccepted)) // terminal is immutable

	require.NoError(t, stream.settleTurn(ctx, turnID, lifecycleOutcomeFor(acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil)))
	settled := len(conn.Updates())
	require.NoError(t, stream.settleTurn(ctx, turnID, lifecycleOutcome{outcome: lifecycle.OutcomeFailed}))
	require.Len(t, conn.Updates(), settled, "final settlement latch must emit exactly once")
}

// TestSessionStreamActionEventsRestateTheWholeStoredRecord pins what this
// adapter's own action call sites state. The render is patch-faithful, so an
// omitted member is now a member the host is never told about; every action this
// adapter emits is a complete sight of one, on the resolution as much as on the
// first, and a call site that started restating less would silently narrow the
// stream rather than fail anywhere.
func TestSessionStreamActionEventsRestateTheWholeStoredRecord(t *testing.T) {
	ctx := t.Context()
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))

	_, err := stream.dispatch(
		ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c", RunID: "run-1"}, "nonce", func() error { return nil })
	require.NoError(t, err)

	action, err := writePreparedActionForTest(ctx, stream, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)
	require.NoError(t, stream.resolveAction(ctx, action, lifecycle.ActionAccepted))

	states := []string{}

	for _, update := range conn.Updates() {
		envelope, ok := update.Meta[lifecycle.MetaKey].(map[string]any)
		require.True(t, ok)

		event := requireAnyMap(t, envelope["event"])
		if event["type"] != string(lifecycle.EventActionUpdate) {
			continue
		}

		emitted := requireAnyMap(t, event["action"])
		state, ok := emitted["state"].(string)
		require.True(t, ok)

		states = append(states, state)

		require.Equal(t, map[string]any{
			"actionId":         action.ActionID,
			"kind":             string(lifecycle.ActionPermission),
			"state":            state,
			"owner":            map[string]any{"type": string(lifecycle.OwnerTurn), "id": stream.stream.State().Turns[0].TurnID},
			"runId":            "run-1",
			"blocksForeground": true,
		}, emitted)
	}

	require.Equal(t,
		[]string{string(lifecycle.ActionPending), string(lifecycle.ActionAccepted)}, states,
		"both sights of the action reached the host")
}

// TestSessionStreamIncarnationLossTerminalizesOwnedWork proves a process loss
// fails its outstanding action and active turn before a fresh incarnation opens.
func TestSessionStreamIncarnationLossTerminalizesOwnedWork(t *testing.T) {
	ctx := t.Context()
	_, _, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))
	oldID := stream.stream.ID()
	turnID, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "submission-1", ClientNonce: "client-1"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	_, err = writePreparedActionForTest(ctx, stream, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)
	require.NoError(t, stream.loseIncarnation(ctx))
	require.False(t, stream.live)
	require.Nil(t, stream.turn)
	require.Empty(t, stream.actions)
	require.NoError(t, stream.settleTurn(ctx, turnID, lifecycleOutcome{outcome: lifecycle.OutcomeSuccess}))
	require.NoError(t, stream.incarnate(ctx))
	require.NotEqual(t, oldID, stream.stream.ID(), "new native process must get a new incarnation")
}

func TestSessionStreamRoutingRefusalsAndEmissionFailureLatch(t *testing.T) {
	ctx := t.Context()
	_, conn, stream := newLifecycleStreamTestSession(t)

	require.NoError(t, stream.incarnate(ctx))
	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "not-sent", ClientNonce: "client-1"}, "nonce", func() error { return errors.New("native refused") })
	require.ErrorContains(t, err, "native refused")
	require.Nil(t, stream.turn)

	_, err = stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client-1"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	update, err := writePreparedActionForTest(ctx, stream, "wrong-incarnation-route", lifecycle.ActionPermission)
	require.NoError(t, err)
	require.Empty(t, update.ActionID)
	require.Nil(t, stream.correlation(update))

	conn.sessionUpdateErr = errors.New("host disconnected")
	_, err = writePreparedActionForTest(ctx, stream, "nonce", lifecycle.ActionPermission)
	require.ErrorContains(t, err, "lifecycle delivery failed")
	latched := stream.lost
	conn.sessionUpdateErr = nil
	require.ErrorIs(t, stream.loseIncarnation(ctx), latched)
	require.NoError(t, stream.incarnate(ctx), "the lost incarnation never holds the next one")
}

func TestSessionStreamCloseFenceAndHelpers(t *testing.T) {
	ctx := t.Context()
	_, _, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))
	turnID, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client-1"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	update, err := writePreparedActionForTest(ctx, stream, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)
	correlation := stream.correlation(update)
	require.Contains(t, correlation, lifecycle.MetaKey)
	require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))
	require.True(t, stream.fenced)
	require.False(t, stream.live)
	require.NoError(t, stream.settleTurn(ctx, turnID, lifecycleOutcome{}))
	require.Error(t, stream.incarnate(ctx))

	base := map[string]any{"vendor": true}
	merged := withLifecycleMeta(base, correlation)
	require.Equal(t, true, merged["vendor"])
	require.Contains(t, merged, lifecycle.MetaKey)
	require.NotContains(t, base, lifecycle.MetaKey)
	require.Equal(t, base, withLifecycleMeta(base, nil))
	require.Contains(t, withLifecycleMeta(nil, correlation), lifecycle.MetaKey)

	require.Equal(t, lifecycle.ActionCancelled, actionEndFor(lifecycle.OutcomeCancelled))
	require.Equal(t, lifecycle.ActionFailed, actionEndFor(lifecycle.OutcomeFailed))
	require.Equal(t, lifecycle.StopReasonCancelled, stopReasonForOutcome(lifecycle.OutcomeCancelled))
	require.Empty(t, stopReasonForOutcome(lifecycle.OutcomeFailed))
}

func TestLifecycleOutcomeMappings(t *testing.T) {
	for _, tc := range []struct {
		reason acp.StopReason
		want   lifecycle.Outcome
	}{
		{acp.StopReasonCancelled, lifecycle.OutcomeCancelled},
		{acp.StopReasonMaxTokens, lifecycle.OutcomeLimit},
		{acp.StopReasonMaxTurnRequests, lifecycle.OutcomeLimit},
		{acp.StopReasonRefusal, lifecycle.OutcomeRefused},
		{acp.StopReasonEndTurn, lifecycle.OutcomeSuccess},
	} {
		require.Equal(t, tc.want, lifecycleOutcomeFor(acp.PromptResponse{StopReason: tc.reason}, nil).outcome)
	}
	require.Equal(t, lifecycle.OutcomeFailed, lifecycleOutcomeFor(acp.PromptResponse{}, errors.New("failed")).outcome)
}

func TestSessionStreamBoundaryAndErrorBranches(t *testing.T) {
	ctx := t.Context()
	var absent *sessionStream
	require.NoError(t, absent.incarnate(ctx))
	called := false
	_, err := absent.dispatch(ctx, lifecycle.Submission{}, "", func() error {
		called = true

		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, absent.settleTurn(ctx, "turn", lifecycleOutcome{}))
	require.NoError(t, absent.resolveAction(ctx, lifecycle.ActionUpdate{ActionID: "action"}, lifecycle.ActionFailed))
	require.NoError(t, absent.loseIncarnation(ctx))
	absent.abandonIncarnation()
	committed := false
	require.NoError(t, absent.settleClose(ctx, func() error {
		committed = true

		return nil
	}))
	require.True(t, committed, "a connection that carries no lifecycle answer still owes the store its prefix")
	absent.fenceClose()

	session, _, stream := newLifecycleStreamTestSession(t)
	require.Same(t, stream, session.lifecycleStream())
	require.NoError(t, stream.loseIncarnation(ctx)) // no live incarnation
	require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))

	_, _, abandoned := newLifecycleStreamTestSession(t)
	require.NoError(t, abandoned.incarnate(ctx))
	_, err = abandoned.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "n", func() error { return nil })
	require.NoError(t, err)
	_, err = writePreparedActionForTest(ctx, abandoned, "n", lifecycle.ActionPermission)
	require.NoError(t, err)
	abandoned.abandonIncarnation()
	require.False(t, abandoned.live)
	require.Nil(t, abandoned.turn)
	require.Empty(t, abandoned.actions)

	_, _, uncommitted := newLifecycleStreamTestSession(t)
	require.NoError(t, uncommitted.incarnate(ctx))
	require.ErrorContains(t,
		uncommitted.settleClose(ctx, closeCommit(errors.New("snapshot commit failed"))), "snapshot commit failed")
	require.True(t, uncommitted.fenced)

	noAgent := (&agentSession{}).lifecycleStream()
	require.Nil(t, noAgent)
	require.Nil(t, (&agentSession{agent: NewAgent()}).lifecycleStream())
}

func TestSessionStreamPropagatesIdentityAndProtocolFailures(t *testing.T) {
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))
	conn.sessionUpdateErr = errors.New("acceptance delivery")
	_, err := stream.dispatch(t.Context(), lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "n", func() error { return nil })
	require.ErrorContains(t, err, "lifecycle delivery failed")

	// A fenced stream can announce no acceptance, so the frame is never written:
	// the refusal is preflighted rather than discovered after the harness has the
	// work.
	_, _, fenced := newLifecycleStreamTestSession(t)
	fenced.fenceClose()
	called := false
	_, err = fenced.dispatch(t.Context(), lifecycle.Submission{}, "", func() error {
		called = true

		return nil
	})
	require.False(t, called)
	require.Error(t, err)
}

func TestSessionStreamFailureAtEachSettlementBoundary(t *testing.T) {
	ctx := t.Context()
	makeActive := func(t *testing.T) (*recordingAgentClient, *sessionStream, string, lifecycle.ActionUpdate) {
		t.Helper()

		_, conn, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(ctx))
		turn, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "n", func() error { return nil })
		require.NoError(t, err)
		action, err := writePreparedActionForTest(ctx, stream, "n", lifecycle.ActionPermission)
		require.NoError(t, err)

		return conn, stream, turn, action
	}

	t.Run("action resolution", func(t *testing.T) {
		conn, stream, _, action := makeActive(t)
		conn.sessionUpdateErr = errors.New("resolution delivery")
		require.ErrorContains(t, stream.resolveAction(ctx, action, lifecycle.ActionAccepted), "lifecycle delivery failed")
	})
	t.Run("turn terminalizes action", func(t *testing.T) {
		conn, stream, turn, _ := makeActive(t)
		conn.sessionUpdateErr = errors.New("terminal action delivery")
		require.ErrorContains(t, stream.settleTurn(ctx, turn, lifecycleOutcome{outcome: lifecycle.OutcomeFailed}), "lifecycle delivery failed")
	})
	t.Run("turn idle", func(t *testing.T) {
		conn, stream, turn, action := makeActive(t)
		require.NoError(t, stream.resolveAction(ctx, action, lifecycle.ActionDeclined))
		conn.sessionUpdateErr = errors.New("idle delivery")
		require.ErrorContains(t, stream.settleTurn(ctx, turn, lifecycleOutcome{
			stopReason: lifecycle.StopReasonEndTurn,
			outcome:    lifecycle.OutcomeSuccess,
		}), "lifecycle delivery failed")
	})
	t.Run("loss idle", func(t *testing.T) {
		conn, stream, _, action := makeActive(t)
		require.NoError(t, stream.resolveAction(ctx, action, lifecycle.ActionDeclined))
		conn.sessionUpdateErr = errors.New("loss delivery")
		require.ErrorContains(t, stream.loseIncarnation(ctx), "lifecycle delivery failed")
	})
	t.Run("close terminal action", func(t *testing.T) {
		conn, stream, _, _ := makeActive(t)
		conn.sessionUpdateErr = errors.New("close delivery")
		require.ErrorContains(t, stream.settleClose(ctx, closeCommit(nil)), "lifecycle delivery failed")
		require.True(t, stream.fenced)
	})
}

func TestSessionStreamIncarnateFailsWithoutEntropy(t *testing.T) {
	previous := uuidRandom
	uuidRandom = strings.NewReader("")
	t.Cleanup(func() { uuidRandom = previous })

	_, conn, stream := newLifecycleStreamTestSession(t)
	require.ErrorContains(t, stream.incarnate(t.Context()), "read random uuid")
	require.False(t, stream.live)
	require.Empty(t, conn.Updates())
}

func TestSessionStreamAnnouncementAfterFailureLatch(t *testing.T) {
	_, conn, stream, turnCtx := newLifecycleActionSession(t, "nonce")

	conn.sessionUpdateErr = errors.New("host disconnected")
	_, err := writePreparedActionForTest(turnCtx, stream, "nonce", lifecycle.ActionPermission)
	require.ErrorContains(t, err, "lifecycle delivery failed")
	latched := stream.lost

	conn.sessionUpdateErr = nil
	_, err = writePreparedActionForTest(turnCtx, stream, "nonce", lifecycle.ActionPermission)
	require.ErrorIs(t, err, latched)
}

func TestSessionStreamCloseSettlesAuthoritativeQuiescence(t *testing.T) {
	ctx := t.Context()
	agent := NewAgent(WithHostAuthority(newFakeHostAuthority()))
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	_, err := agent.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.True(t, agent.negotiatedLifecycle().AuthoritativeQuiescence)

	session := &agentSession{agent: agent, id: "session-quiescent"}
	stream := session.lifecycleStream()
	require.NoError(t, stream.incarnate(ctx))
	_, err = stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c", RunID: "r"}, "nonce", func() error { return nil })
	require.NoError(t, err)

	require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))
	require.True(t, stream.fenced)

	updates := conn.Updates()
	require.NotEmpty(t, updates)
	envelope := requireAnyMap(t, updates[len(updates)-1].Meta[lifecycle.MetaKey])
	event := requireAnyMap(t, envelope["event"])
	require.Equal(t, string(lifecycle.EventQuiescenceUpdate), event["type"])
	require.Equal(t, true, event["quiescent"])
	require.Equal(t, string(lifecycle.ProofClassProcessContainment), event["source"])
	watermark, ok := event["watermark"].(uint64)
	require.True(t, ok)
	require.Equal(t, envelope["sequence"], watermark+1)
}

func TestSessionStreamEmissionFailsClosedOnAnUntruthfulEvent(t *testing.T) {
	ctx := t.Context()
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))
	delivered := len(conn.Updates())

	err := stream.emitLocked(ctx, lifecycle.QuiescenceEvent(lifecycle.QuiescenceFact{
		Quiescent: true,
		Source:    lifecycle.ProofClassProcessContainment,
	}))
	require.Error(t, err)
	require.ErrorIs(t, stream.refused, err)
	require.Len(t, conn.Updates(), delivered, "a refused event is never delivered")

	require.ErrorIs(t, stream.incarnate(ctx), err, "a latched stream never continues its sequence")
}

func TestSessionStreamEmissionFailsClosedWithoutAConnection(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))

	session.agent.setConnection(nil)
	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
	require.ErrorContains(t, err, "the ACP connection is unavailable")
	require.Nil(t, stream.turn)

	session.agent.setConnection(conn)
	require.NoError(t, stream.incarnate(ctx), "a reachable host opens the next incarnation")
}

// TestSessionStreamSettlementIgnoresACancelledRequestContext proves settlement
// emissions are detached from the request that asked for them. A host that
// withdraws its prompt un-happens nothing the turn already did, so the terminal
// idle and the retirement of the incarnation still reach it; live work asked for
// under the same withdrawn request is still refused, which is what makes the
// detachment settlement's and not the whole stream's.
func TestSessionStreamSettlementIgnoresACancelledRequestContext(t *testing.T) {
	withdrawnTurn := func(t *testing.T) (*recordingAgentClient, *sessionStream, string, context.Context) {
		t.Helper()

		ctx := t.Context()
		_, conn, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(ctx))

		turnID, err := stream.dispatch(
			ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
		require.NoError(t, err)

		withdrawn, cancel := context.WithCancel(ctx)
		cancel()

		return conn, stream, turnID, withdrawn
	}

	t.Run("live work still honors its request", func(t *testing.T) {
		_, stream, _, withdrawn := withdrawnTurn(t)

		_, err := writePreparedActionForTest(withdrawn, stream, "nonce", lifecycle.ActionPermission)
		require.ErrorContains(t, err, "lifecycle delivery failed",
			"a request the host withdrew announces no new action against it")
	})
	t.Run("settlement does not", func(t *testing.T) {
		conn, stream, turnID, withdrawn := withdrawnTurn(t)

		require.NoError(t, stream.settleTurn(withdrawn, turnID, lifecycleOutcome{
			stopReason: lifecycle.StopReasonEndTurn,
			outcome:    lifecycle.OutcomeSuccess,
		}))
		require.NoError(t, stream.loseIncarnation(withdrawn))
		require.NoError(t, stream.settleClose(withdrawn, closeCommit(nil)))

		require.Equal(t, []string{
			string(lifecycle.EventSnapshot),
			string(lifecycle.EventPromptAccepted),
			string(lifecycle.EventStateUpdate),
			string(lifecycle.EventStateUpdate),
		}, lifecycleEventTypes(t, conn))
	})
}

// TestSessionStreamLostDeliveryNeverHoldsTheNextIncarnation proves the delivery
// latch ends with the incarnation it holed. The next incarnation opens on its own
// identity and re-asserts the whole state in its own snapshot, so it owes the host
// nothing the lost one failed to deliver; a refusal, which is about what this
// adapter may state at all, still follows the session.
func TestSessionStreamLostDeliveryNeverHoldsTheNextIncarnation(t *testing.T) {
	ctx := t.Context()
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))

	first := stream.stream.ID()

	conn.sessionUpdateErr = errors.New("host disconnected")
	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
	require.ErrorContains(t, err, "lifecycle delivery failed")
	require.NotNil(t, stream.lost)

	conn.sessionUpdateErr = nil
	require.NoError(t, stream.incarnate(ctx))
	require.Nil(t, stream.lost)
	require.NotEqual(t, first, stream.stream.ID())

	_, err = stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s2", ClientNonce: "c2"}, "nonce", func() error { return nil })
	require.NoError(t, err, "the fresh incarnation speaks again")
}

// TestSessionStreamCloseStatesNoFactOnADeadIncarnation proves settlement facts
// belong to a live incarnation. A stream that never opened one has no identity to
// name, and one whose incarnation cancel already retired has only an identity a
// conforming reducer must refuse as stale, so close emits nothing in either case
// and succeeds on the containment evidence it already holds.
func TestSessionStreamCloseStatesNoFactOnADeadIncarnation(t *testing.T) {
	// The configuration advertises an authoritative quiescence source, so close
	// has a positive fact to state and a dead incarnation has nowhere to state it.
	authoritative := func(t *testing.T) (*recordingAgentClient, *sessionStream) {
		t.Helper()

		_, conn, stream := newAuthoritativeLifecycleStreamTestSession(t)

		return conn, stream
	}

	t.Run("never incarnated", func(t *testing.T) {
		conn, stream := authoritative(t)

		require.NoError(t, stream.settleClose(t.Context(), closeCommit(nil)))
		require.True(t, stream.fenced)
		require.Empty(t, lifecycleEventTypes(t, conn), "a stream with no incarnation states nothing")
	})
	t.Run("incarnation lost", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := authoritative(t)
		require.NoError(t, stream.incarnate(ctx))
		_, err := stream.dispatch(
			ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
		require.NoError(t, err)
		require.NoError(t, stream.loseIncarnation(ctx))

		delivered := lifecycleEventTypes(t, conn)

		require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))
		require.True(t, stream.fenced)
		require.Equal(t, delivered, lifecycleEventTypes(t, conn),
			"a retired identity is never continued by a later settlement")
	})
	t.Run("incarnation abandoned", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := authoritative(t)
		require.NoError(t, stream.incarnate(ctx))
		stream.abandonIncarnation()

		delivered := lifecycleEventTypes(t, conn)

		require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))
		require.Equal(t, delivered, lifecycleEventTypes(t, conn))
	})
}

// TestSessionStreamCloseSettlesWhatHappenedAndCertifiesOnlyWhatCommitted pins
// the close boundary's rung order at the seam a failed commit exposes. The
// containment boundary completed, so the session is over and the fence is
// unconditional; the terminal transitions report how the work that proof
// contained ended, which the store cannot make untrue, so they precede the
// commit and stand. Only the quiescence fact is withheld — it certifies the
// boundary itself, and a boundary whose snapshot the store does not hold is
// exactly the one nothing may certify — and the close fails.
func TestSessionStreamCloseSettlesWhatHappenedAndCertifiesOnlyWhatCommitted(t *testing.T) {
	// liveIncarnation opens the shape the settlement acts on: a live incarnation
	// holding an open turn and one outstanding action.
	liveIncarnation := func(t *testing.T) (*recordingAgentClient, *sessionStream) {
		t.Helper()

		ctx := t.Context()
		_, conn, stream := newAuthoritativeLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(ctx))

		_, err := stream.dispatch(
			ctx, lifecycle.Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "nonce-1", func() error { return nil })
		require.NoError(t, err)

		_, err = writePreparedActionForTest(ctx, stream, "nonce-1", lifecycle.ActionPermission)
		require.NoError(t, err)

		return conn, stream
	}

	t.Run("a failed commit certifies nothing and fails the close", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := liveIncarnation(t)

		before := len(lifecycleEventTypes(t, conn))

		require.ErrorContains(t,
			stream.settleClose(ctx, closeCommit(errors.New("snapshot commit failed"))), "snapshot commit failed")
		require.True(t, stream.fenced, "the containment boundary completed, so the session is over either way")
		require.Equal(t, []string{
			string(lifecycle.EventActionUpdate),
			string(lifecycle.EventStateUpdate),
		}, lifecycleEventTypes(t, conn)[before:],
			"the transitions precede the commit and report an end the store cannot make untrue")

		state := stream.stream.State()
		require.Len(t, state.Actions, 1)
		require.True(t, state.Actions[0].State.Terminal(), "the boundary cancelled what the session still held")
		require.Len(t, state.Turns, 1)
		require.True(t, state.Turns[0].Terminal, "the still-open turn received its terminal idle")
		require.False(t, state.Quiescence.Certified,
			"no fact certifies a boundary whose snapshot the store does not hold")
	})

	t.Run("a landed commit settles everything", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := liveIncarnation(t)

		before := len(lifecycleEventTypes(t, conn))

		require.NoError(t, stream.settleClose(ctx, closeCommit(nil)))
		require.True(t, stream.fenced)
		require.Equal(t, []string{
			string(lifecycle.EventActionUpdate),
			string(lifecycle.EventStateUpdate),
			string(lifecycle.EventQuiescenceUpdate),
		}, lifecycleEventTypes(t, conn)[before:],
			"a durable close cancels what it held, reports the terminal idle, and states the fact")

		state := stream.stream.State()
		require.True(t, state.Actions[0].State.Terminal())
		require.True(t, state.Turns[0].Terminal)
		require.Equal(t, lifecycle.OutcomeCancelled, state.Turns[0].Outcome)
		require.True(t, state.Quiescence.Certified)
	})
}

// mirroredSessionUUID names the transcript the close-fenced cases mirror.
const mirroredSessionUUID = "11111111-1111-4111-8111-111111111111"

// closeFencedSession builds the shape a cancel-then-close arrives at: an open
// incarnation holding a live turn and an outstanding action, and a native pump
// carrying one transcript frame the store does not hold yet. Losing the
// incarnation from here is what a cancel does, and everything the close still
// owes runs after that loss.
//
// The configuration is the authoritative one, so this session has a positive
// settlement fact to state and the case that asserts none was stated can fail.
func closeFencedSession(t *testing.T, store SessionStore) (*agentSession, *recordingAgentClient, *sessionStream) {
	t.Helper()

	session, conn, stream := newAuthoritativeLifecycleStreamTestSession(t)
	projects := filepath.Join(t.TempDir(), "projects")
	session.mirror = &sessionMirror{log: session.agent.log, store: store, projectsDir: projects}

	ctx := t.Context()
	require.NoError(t, stream.incarnate(ctx))

	_, err := stream.dispatch(
		ctx, lifecycle.Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "nonce-1", func() error { return nil })
	require.NoError(t, err)

	_, err = writePreparedActionForTest(ctx, stream, "nonce-1", lifecycle.ActionPermission)
	require.NoError(t, err)

	session.nativePumpHandle().work <- nativePumpWork{frame: &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(projects, "project", mirroredSessionUUID+".jsonl"),
		Entries:  []json.RawMessage{json.RawMessage(`{"type":"user"}`)},
	}}
	t.Cleanup(session.stopNativePump)

	return session, conn, stream
}

// TestCloseSettlesADeadIncarnationDurablyAndTruthfully pins the non-emission
// rungs of the close-fenced order, which are the ones a fence does not excuse.
// The emissions belong to a live incarnation and a fenced one gets none, but the
// containment proof and the durable commits run unconditionally: a close that
// skipped the commit because there was nobody left to tell would lose the prefix
// the store is owed, and a close that swallowed a failed commit would report a
// boundary the store cannot back. What the loss already terminalized is also
// final — an entity that failed is never rewritten as cancelled by the close
// that followed it, because the host was already told how that work ended.
func TestCloseSettlesADeadIncarnationDurablyAndTruthfully(t *testing.T) {
	t.Run("the durable commit lands behind a fence", func(t *testing.T) {
		ctx := t.Context()
		store := NewInMemorySessionStore()
		session, conn, stream := closeFencedSession(t, store)

		require.NoError(t, stream.loseIncarnation(ctx))

		fenced := lifecycleEventTypes(t, conn)

		require.NoError(t, session.settleSessionClose(ctx, nil))
		require.True(t, stream.fenced)
		require.Equal(t, fenced, lifecycleEventTypes(t, conn), "a fenced incarnation is told nothing more")

		entries, err := store.Load(ctx, SessionKey{SessionID: mirroredSessionUUID})
		require.NoError(t, err)
		require.Len(t, entries, 1, "the prefix the close owes the store lands whether or not anyone is left to tell")
	})

	t.Run("a failed commit still fails the close", func(t *testing.T) {
		ctx := t.Context()
		store := &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("prefix append failed")}
		session, _, stream := closeFencedSession(t, store)

		require.NoError(t, stream.loseIncarnation(ctx))

		err := session.settleSessionClose(ctx, nil)
		require.ErrorContains(t, err, "prefix append failed")
		require.True(t, stream.fenced, "the session is over even where its last commit was not")
	})

	t.Run("work the loss failed is never rewritten as cancelled", func(t *testing.T) {
		ctx := t.Context()
		session, conn, stream := closeFencedSession(t, NewInMemorySessionStore())

		require.NoError(t, stream.loseIncarnation(ctx))
		requireFailedIncarnationRecord(t, stream)

		require.NoError(t, session.settleSessionClose(ctx, nil))
		requireFailedIncarnationRecord(t, stream)

		for _, eventType := range lifecycleEventTypes(t, conn) {
			require.NotEqual(t, string(lifecycle.EventQuiescenceUpdate), eventType,
				"a fenced incarnation certifies nothing")
		}
	})
}

// TestCloseTerminalizesBeforeTheCommitItThenFails pins the close boundary's rung
// order where the two rungs can be told apart: a live incarnation, and a store
// that refuses the snapshot the boundary owes it. The containment proof has
// completed, so how the work it contained ended is already true, and the host is
// told it — the outstanding action ends cancelled and the still-open turn gets
// its terminal idle — before the commit is attempted at all. The commit then
// fails, so no quiescence fact certifies a boundary the store cannot back and the
// close reports the store's refusal. Committing first would leave a host with a
// session that is over and a projection still showing its work running.
func TestCloseTerminalizesBeforeTheCommitItThenFails(t *testing.T) {
	ctx := t.Context()
	store := &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("prefix append failed")}
	session, conn, stream := closeFencedSession(t, store)

	before := len(lifecycleEventTypes(t, conn))

	require.ErrorContains(t, session.settleSessionClose(ctx, nil), "prefix append failed")
	require.True(t, stream.fenced)

	require.Equal(t, []string{
		string(lifecycle.EventActionUpdate),
		string(lifecycle.EventStateUpdate),
	}, lifecycleEventTypes(t, conn)[before:],
		"the host is told how the contained work ended, and is certified nothing behind a refused commit")

	state := stream.stream.State()
	require.Len(t, state.Actions, 1)
	require.Equal(t, lifecycle.ActionCancelled, state.Actions[0].State)
	require.Len(t, state.Turns, 1)
	require.True(t, state.Turns[0].Terminal)
	require.Equal(t, lifecycle.OutcomeCancelled, state.Turns[0].Outcome)
	require.False(t, state.Quiescence.Certified)
}

func TestCloseAdmittedPrefixAbortFencesWithoutQuiescence(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := closeFencedSession(t, NewInMemorySessionStore())

	err := session.settleSessionClose(ctx, &claude.ControllerDataError{
		Kind: claude.ControllerDataTeardownAbort,
	})
	require.NoError(t, err)
	require.True(t, stream.fenced)
	require.False(t, stream.live)

	for _, eventType := range lifecycleEventTypes(t, conn) {
		require.NotEqual(t, string(lifecycle.EventQuiescenceUpdate), eventType)
	}
}

// requireFailedIncarnationRecord asserts the record an incarnation loss wrote:
// its outstanding action and its unsettled turn both ended as failed, which is
// what tells a host a lost end from a contained one.
func requireFailedIncarnationRecord(t *testing.T, stream *sessionStream) {
	t.Helper()

	state := stream.stream.State()

	require.Len(t, state.Actions, 1)
	require.Equal(t, lifecycle.ActionFailed, state.Actions[0].State)
	require.Len(t, state.Turns, 1)
	require.True(t, state.Turns[0].Terminal)
	require.Equal(t, lifecycle.OutcomeFailed, state.Turns[0].Outcome)
}

func agentHoldingALiveIncarnation(t *testing.T, conn agentClient) (*Agent, *sessionStream) {
	t.Helper()

	ctx := t.Context()
	agent := NewAgent(WithHostAuthority(newFakeHostAuthority()))
	agent.setConnection(conn)

	_, err := agent.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.True(t, agent.negotiatedLifecycle().AuthoritativeQuiescence)

	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, client.Start(ctx))

	session := &agentSession{
		agent:  agent,
		id:     "session-1",
		client: client,
		turn:   make(chan struct{}, sessionTurnCapacity),
	}
	agent.sessions[session.id] = session
	t.Cleanup(session.stopNativePump)

	stream := session.lifecycleStream()
	require.NotNil(t, stream)
	require.NoError(t, stream.incarnate(ctx))

	_, err = stream.dispatch(
		ctx, lifecycle.Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "nonce-1", func() error { return nil })
	require.NoError(t, err)

	_, err = writePreparedActionForTest(ctx, stream, "nonce-1", lifecycle.ActionPermission)
	require.NoError(t, err)

	return agent, stream
}

// TestAgentShutdownDeliversItsFinalLifecycleEmissions pins that the connection
// outlives the close ladders that run on it. Agent shutdown is a real close
// boundary: it terminalizes the actions the session still owns, reports the
// cancelled cycle's terminal idle, and states the quiescence its completed
// containment proved. Discarding the carrier before those ladders run would make
// every one of them undeliverable and turn a clean shutdown into a lifecycle
// violation nobody committed.
func TestAgentShutdownDeliversItsFinalLifecycleEmissions(t *testing.T) {
	conn := newRecordingAgentClient()
	agent, stream := agentHoldingALiveIncarnation(t, conn)

	before := len(lifecycleEventTypes(t, conn))

	require.NoError(t, agent.Close(), "a shutdown that delivered everything it owed reports no failure")
	require.True(t, stream.fenced)

	require.Equal(t, []string{
		string(lifecycle.EventActionUpdate),
		string(lifecycle.EventStateUpdate),
		string(lifecycle.EventQuiescenceUpdate),
	}, lifecycleEventTypes(t, conn)[before:], "the close ladder's own emissions reached the host")
}

// TestServeReturnsNilOnANormalPeerClose drives the same shutdown through Serve,
// which is where the discarded connection actually surfaced: the peer hangs up,
// the loop returns nil, and the deferred agent close overwrites that nil with
// whatever it reports. A clean peer close owes the caller no error.
func TestServeReturnsNilOnANormalPeerClose(t *testing.T) {
	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	agent, stream := agentHoldingALiveIncarnation(t, newRecordingAgentClient())
	newServeAgent = func(...Option) *Agent { return agent }

	require.NoError(t, Serve(t.Context(), bytes.NewBuffer(nil), io.Discard))
	require.True(t, stream.fenced, "the peer's hang-up still ran the close boundary")
}

// TestAgentShutdownSettlesCleanlyAfterTheHostHungUp pins the other side of that
// same reordering. Keeping the connection installed means the close ladder now
// really tries to deliver, and on a peer that already went away that delivery
// fails. It is still loss — the stream latches and fences — but it is the peer's
// hang-up, not this adapter holing its own stream, so the close reports no
// failure. A delivery that failed while the peer was still reading is the
// opposite fact and keeps the violation it earned.
func TestAgentShutdownSettlesCleanlyAfterTheHostHungUp(t *testing.T) {
	t.Run("the peer is already gone", func(t *testing.T) {
		conn := newRecordingAgentClient()
		agent, stream := agentHoldingALiveIncarnation(t, conn)

		// The host hangs up with the incarnation open: its reader loop ends, and
		// every write this adapter still owes it fails from here on.
		conn.sessionUpdateErr = errors.New("write: broken pipe")
		close(conn.done)

		require.NoError(t, agent.Close(), "a hang-up on the far end is not this adapter's failure")
		require.True(t, stream.fenced, "the boundary still fenced the session it contained")

		stream.mu.Lock()
		require.ErrorIs(t, stream.lost, errLifecyclePeerGone, "the incarnation still latched")
		stream.mu.Unlock()
	})

	t.Run("the peer is still reading", func(t *testing.T) {
		conn := newRecordingAgentClient()
		agent, stream := agentHoldingALiveIncarnation(t, conn)
		conn.sessionUpdateErr = errors.New("host disconnected")

		err := agent.Close()
		require.ErrorContains(t, err, "claude_lifecycle_violation")
		require.ErrorContains(t, err, "lifecycle delivery failed")
		require.True(t, stream.fenced)
	})
}

// TestCloseThatCouldNotContainStatesNothingAboutALiveIncarnation pins the branch
// the fenced-close tests cannot reach. Those settle a stream a loss already
// ended, where every emission rung is skipped because the incarnation is gone;
// this one is live, holding an unsettled turn and a blocking action, and the
// boundary still terminalizes nothing and states no fact.
//
// It is not a fenced stream's silence — it is a refusal. The adapter has just
// proved it cannot contain this session's descendants, so declaring their work
// terminal would make their next real event a post-terminal mutation, and a
// quiescence fact would certify a vacancy nothing established. The store is left
// out of it for the same reason: nothing new is committed, which is why a store
// that would fail a commit does not fail this close. What does happen is the
// fence, and the error the caller gets is the containment failure itself.
func TestCloseThatCouldNotContainStatesNothingAboutALiveIncarnation(t *testing.T) {
	ctx := t.Context()

	// A store that fails every append is how "commits nothing new" is asserted:
	// the pump's own outbox is asynchronous, so an empty store proves nothing,
	// while a commit barrier this close never took cannot report the failure.
	store := &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("prefix append failed")}
	session, conn, stream := closeFencedSession(t, store)

	session.client = claude.NewClient(nil, claude.Options{},
		&closeErrTransport{Transport: newFakeClaudeTransport(), err: ErrContainmentIncomplete})
	require.NoError(t, session.client.Start(ctx))

	before := lifecycleEventTypes(t, conn)
	live := stream.stream.State()
	require.Len(t, live.Actions, 1)
	require.False(t, live.Actions[0].State.Terminal(), "the action is nonterminal when the boundary fails")
	require.Len(t, live.Turns, 1)
	require.False(t, live.Turns[0].Terminal)

	err := session.Close(ctx)
	require.ErrorIs(t, err, ErrContainmentIncomplete,
		"the close returns the containment failure and invents nothing beside it")
	require.NotContains(t, err.Error(), "prefix append failed",
		"a boundary that did not complete commits nothing new")

	require.Equal(t, before, lifecycleEventTypes(t, conn),
		"nothing is terminalized and no quiescence fact is stated")
	require.Equal(t, live.Actions, stream.stream.State().Actions,
		"work the adapter cannot contain is never declared terminal")
	require.Equal(t, live.Turns, stream.stream.State().Turns)

	stream.mu.Lock()
	require.True(t, stream.fenced, "the stream is fenced whether or not the boundary completed")
	require.False(t, stream.live)
	stream.mu.Unlock()
}
