package claudeacp

import (
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
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
	latched := stream.failure
	conn.sessionUpdateErr = nil
	require.ErrorIs(t, stream.loseIncarnation(ctx), latched)
	require.ErrorIs(t, stream.incarnate(ctx), latched)
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

	_, _, fenced := newLifecycleStreamTestSession(t)
	require.NoError(t, fenced.settleClose(t.Context(), false, false))
	called := false
	_, err = fenced.dispatch(t.Context(), lifecycle.Submission{}, "", func() error {
		called = true

		return nil
	})
	require.True(t, called)
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
