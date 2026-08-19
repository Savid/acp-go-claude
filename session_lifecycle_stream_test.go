package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	agent := NewAgent()
	agent.containmentMode = RuntimeContainmentAuthoritative
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

	return session, conn, stream, withTurnRoute(ctx, nonce)
}

type lifecycleActionFailingClient struct {
	*recordingAgentClient
	state lifecycle.ActionState
	err   error
}

func (c *lifecycleActionFailingClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any)
	if ok {
		event, eventOK := envelope["event"].(map[string]any)
		action, actionOK := event["action"].(map[string]any)
		if eventOK && actionOK && event["type"] == string(lifecycle.EventActionUpdate) && action["state"] == string(c.state) {
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

	first, err := stream.announceAction(ctx, "nonce-1", lifecycle.ActionPermission)
	require.NoError(t, err)
	second, err := stream.announceAction(ctx, "nonce-1", lifecycle.ActionElicitation)
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

// TestSessionStreamIncarnationLossTerminalizesOwnedWork proves a process loss
// fails its outstanding action and active turn before a fresh incarnation opens.
func TestSessionStreamIncarnationLossTerminalizesOwnedWork(t *testing.T) {
	ctx := t.Context()
	_, _, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))
	oldID := stream.stream.ID()
	turnID, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "submission-1", ClientNonce: "client-1"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	_, err = stream.announceAction(ctx, "nonce", lifecycle.ActionPermission)
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
	update, err := stream.announceAction(ctx, "wrong-incarnation-route", lifecycle.ActionPermission)
	require.NoError(t, err)
	require.Empty(t, update.ActionID)
	require.Nil(t, stream.correlation(update))

	conn.sessionUpdateErr = errors.New("host disconnected")
	_, err = stream.announceAction(ctx, "nonce", lifecycle.ActionPermission)
	require.ErrorContains(t, err, "host disconnected")
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
	update, err := stream.announceAction(ctx, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)
	correlation := stream.correlation(update)
	require.Contains(t, correlation, lifecycle.MetaKey)
	require.NoError(t, stream.settleClose(ctx, true, true))
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
	require.NoError(t, absent.settleClose(ctx, true, true))

	session, _, stream := newLifecycleStreamTestSession(t)
	require.Same(t, stream, session.lifecycleStream())
	require.NoError(t, stream.loseIncarnation(ctx)) // no live incarnation
	require.NoError(t, stream.settleClose(ctx, false, true))

	_, _, abandoned := newLifecycleStreamTestSession(t)
	require.NoError(t, abandoned.incarnate(ctx))
	_, err = abandoned.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "n", func() error { return nil })
	require.NoError(t, err)
	_, err = abandoned.announceAction(ctx, "n", lifecycle.ActionPermission)
	require.NoError(t, err)
	abandoned.abandonIncarnation()
	require.False(t, abandoned.live)
	require.Nil(t, abandoned.turn)
	require.Empty(t, abandoned.actions)

	_, _, uncommitted := newLifecycleStreamTestSession(t)
	require.NoError(t, uncommitted.incarnate(ctx))
	require.NoError(t, uncommitted.settleClose(ctx, true, false))

	noAgent := (&agentSession{}).lifecycleStream()
	require.Nil(t, noAgent)
	require.Nil(t, (&agentSession{agent: NewAgent()}).lifecycleStream())
}

func TestSessionStreamPropagatesIdentityAndProtocolFailures(t *testing.T) {
	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))
	conn.sessionUpdateErr = errors.New("acceptance delivery")
	_, err := stream.dispatch(t.Context(), lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "n", func() error { return nil })
	require.ErrorContains(t, err, "acceptance delivery")

	// A fenced stream can announce no acceptance, so the frame is never written:
	// the refusal is preflighted rather than discovered after the harness has the
	// work.
	_, _, fenced := newLifecycleStreamTestSession(t)
	require.NoError(t, fenced.settleClose(t.Context(), false, false))
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
		action, err := stream.announceAction(ctx, "n", lifecycle.ActionPermission)
		require.NoError(t, err)

		return conn, stream, turn, action
	}

	t.Run("action resolution", func(t *testing.T) {
		conn, stream, _, action := makeActive(t)
		conn.sessionUpdateErr = errors.New("resolution delivery")
		require.ErrorContains(t, stream.resolveAction(ctx, action, lifecycle.ActionAccepted), "resolution delivery")
	})
	t.Run("turn terminalizes action", func(t *testing.T) {
		conn, stream, turn, _ := makeActive(t)
		conn.sessionUpdateErr = errors.New("terminal action delivery")
		require.ErrorContains(t, stream.settleTurn(ctx, turn, lifecycleOutcome{outcome: lifecycle.OutcomeFailed}), "terminal action delivery")
	})
	t.Run("turn idle", func(t *testing.T) {
		conn, stream, turn, action := makeActive(t)
		require.NoError(t, stream.resolveAction(ctx, action, lifecycle.ActionDeclined))
		conn.sessionUpdateErr = errors.New("idle delivery")
		require.ErrorContains(t, stream.settleTurn(ctx, turn, lifecycleOutcome{
			stopReason: lifecycle.StopReasonEndTurn,
			outcome:    lifecycle.OutcomeSuccess,
		}), "idle delivery")
	})
	t.Run("loss idle", func(t *testing.T) {
		conn, stream, _, action := makeActive(t)
		require.NoError(t, stream.resolveAction(ctx, action, lifecycle.ActionDeclined))
		conn.sessionUpdateErr = errors.New("loss delivery")
		require.ErrorContains(t, stream.loseIncarnation(ctx), "loss delivery")
	})
	t.Run("close terminal action", func(t *testing.T) {
		conn, stream, _, _ := makeActive(t)
		conn.sessionUpdateErr = errors.New("close delivery")
		require.ErrorContains(t, stream.settleClose(ctx, true, true), "close delivery")
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
	_, err := stream.announceAction(turnCtx, "nonce", lifecycle.ActionPermission)
	require.ErrorContains(t, err, "host disconnected")
	latched := stream.lost

	conn.sessionUpdateErr = nil
	_, err = stream.announceAction(turnCtx, "nonce", lifecycle.ActionPermission)
	require.ErrorIs(t, err, latched)
}

func TestSessionStreamCloseSettlesAuthoritativeQuiescence(t *testing.T) {
	ctx := t.Context()
	agent := NewAgent()
	agent.containmentMode = RuntimeContainmentAuthoritative
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

	require.NoError(t, stream.settleClose(ctx, true, true))
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

		_, err := stream.announceAction(withdrawn, "nonce", lifecycle.ActionPermission)
		require.ErrorContains(t, err, context.Canceled.Error(),
			"a request the host withdrew announces no new action against it")
	})
	t.Run("settlement does not", func(t *testing.T) {
		conn, stream, turnID, withdrawn := withdrawnTurn(t)

		require.NoError(t, stream.settleTurn(withdrawn, turnID, lifecycleOutcome{
			stopReason: lifecycle.StopReasonEndTurn,
			outcome:    lifecycle.OutcomeSuccess,
		}))
		require.NoError(t, stream.loseIncarnation(withdrawn))
		require.NoError(t, stream.settleClose(withdrawn, true, true))

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
	require.ErrorContains(t, err, "host disconnected")
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

		require.NoError(t, stream.settleClose(t.Context(), true, true))
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

		require.NoError(t, stream.settleClose(ctx, true, true))
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

		require.NoError(t, stream.settleClose(ctx, true, true))
		require.Equal(t, delivered, lifecycleEventTypes(t, conn))
	})
}

// TestSessionStreamCloseSettlesNothingBehindAFailedCommit pins the rung
// durability outranks. The containment boundary completed, so the session is over
// and the fence is unconditional; the durable commit did not, so the close states
// nothing at all — no action ends as cancelled, no terminal idle reports the
// cycle, and no quiescence fact certifies a boundary the store cannot back. The
// incarnation ends unsettled and the next one's snapshot asserts the truthful
// state.
//
// The stream is driven directly because the production close cannot reach this
// combination: the close barrier admits the settlement only once the session's
// turn slot is free, and a turn releases that slot with its actions terminalized
// and its turn ended — and where the turn's own commit failed, with its
// incarnation already abandoned. The guard is therefore hardening, and this is the
// only place that can hold it.
func TestSessionStreamCloseSettlesNothingBehindAFailedCommit(t *testing.T) {
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

		_, err = stream.announceAction(ctx, "nonce-1", lifecycle.ActionPermission)
		require.NoError(t, err)

		return conn, stream
	}

	t.Run("a failed commit settles nothing", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := liveIncarnation(t)

		delivered := lifecycleEventTypes(t, conn)

		require.NoError(t, stream.settleClose(ctx, true, false))
		require.True(t, stream.fenced, "the containment boundary completed, so the session is over either way")
		require.Equal(t, delivered, lifecycleEventTypes(t, conn),
			"a close whose durable prefix the store does not hold terminalizes nothing and certifies nothing")

		state := stream.stream.State()
		require.Len(t, state.Actions, 1)
		require.False(t, state.Actions[0].State.Terminal(), "the action the close could not settle stays live")
		require.Len(t, state.Turns, 1)
		require.False(t, state.Turns[0].Terminal, "the incarnation ends unsettled rather than idle")
		require.False(t, state.Quiescence.Certified)
	})

	t.Run("a landed commit settles everything", func(t *testing.T) {
		ctx := t.Context()
		conn, stream := liveIncarnation(t)

		before := len(lifecycleEventTypes(t, conn))

		require.NoError(t, stream.settleClose(ctx, true, true))
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
func closeFencedSession(t *testing.T, store SessionStore) (*agentSession, *recordingAgentClient, *sessionStream) {
	t.Helper()

	session, conn, stream := newLifecycleStreamTestSession(t)
	projects := filepath.Join(t.TempDir(), "projects")
	session.mirror = &sessionMirror{log: session.agent.log, store: store, projectsDir: projects}

	ctx := t.Context()
	require.NoError(t, stream.incarnate(ctx))

	_, err := stream.dispatch(
		ctx, lifecycle.Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "nonce-1", func() error { return nil })
	require.NoError(t, err)

	_, err = stream.announceAction(ctx, "nonce-1", lifecycle.ActionPermission)
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

// ctxHonoringBlockingClient is a host whose delivery never returns on its own
// and only ends when its context does — a wedged connection, which is the one
// thing a detached settlement has no other way out of.
type ctxHonoringBlockingClient struct {
	*recordingAgentClient

	gate chan struct{}
}

func (c *ctxHonoringBlockingClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	select {
	case <-c.gate:
		return c.recordingAgentClient.SessionUpdate(ctx, notification)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ctxHonoringBlockingClient) release() { close(c.gate) }

// TestSettlementEmissionsAreBoundedNotMerelyDetached proves the settlement
// context keeps its detachment and gains a bound. The request is withdrawn, so a
// cancellation-following emission would abandon work that already completed; the
// budget is what stops a wedged host from holding the stream, the turn slot
// behind it, and the close waiting on that slot forever. The expiry is a lost
// delivery like any other, so it fails the caller, latches this incarnation, and
// does not follow the next one.
func TestSettlementEmissionsAreBoundedNotMerelyDetached(t *testing.T) {
	previousTimeout := sessionSettlementTimeout
	t.Cleanup(func() { sessionSettlementTimeout = previousTimeout })
	sessionSettlementTimeout = 50 * time.Millisecond

	_, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))

	turnID, err := stream.dispatch(
		t.Context(),
		lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"},
		"nonce",
		func() error { return nil },
	)
	require.NoError(t, err)

	blocked := &ctxHonoringBlockingClient{recordingAgentClient: conn, gate: make(chan struct{})}
	stream.session.agent.setConnection(blocked)

	withdrawn, cancel := context.WithCancel(context.Background())
	cancel()

	settled := make(chan error, 1)

	go func() {
		settled <- stream.settleTurn(withdrawn, turnID,
			lifecycleOutcomeFor(acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil))
	}()

	var settleErr error

	select {
	case settleErr = <-settled:
	case <-time.After(30 * time.Second):
		blocked.release()
		t.Fatal("settlement never returned: the detached context carries no bound")
	}

	require.ErrorContains(t, settleErr, "claude_lifecycle_violation")
	require.ErrorContains(t, settleErr, context.DeadlineExceeded.Error())

	stream.mu.Lock()
	require.Error(t, stream.lost, "an emission the host never received is loss")
	require.NoError(t, stream.refused, "a wedged host is not something this adapter may not state")
	stream.mu.Unlock()

	// The loss latched the incarnation it holed, not the session: the next
	// incarnation opens on a fresh identity and re-asserts the whole state.
	blocked.release()
	require.NoError(t, stream.incarnate(t.Context()))
}
