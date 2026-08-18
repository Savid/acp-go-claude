package claudeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

// TestNativePumpTerminalBranches exercises shutdown and incarnation observation
// at the session pump boundary, including barriers after the outbox has stopped.
func TestNativePumpTerminalBranches(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	session.stopNativePump() // never started
	pump := newNativePump(session)
	pump.quitOnce.Do(func() { close(pump.quit) })
	<-pump.workDone
	require.NoError(t, pump.barrier(t.Context()))

	require.ErrorIs(t, pump.incarnationError(), claude.ErrMessageStreamClosed)
	require.True(t, pump.incarnationEnded())

	pump.lost = make(chan struct{})
	require.False(t, pump.incarnationEnded())
	close(pump.lost)
	require.True(t, pump.incarnationEnded())

	want := errors.New("native lost")
	pump.err = want
	require.ErrorIs(t, pump.incarnationError(), want)
}

func TestNativePumpBarrierHonorsCancellationAndDrain(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	pump := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, pump.barrier(ctx), context.Canceled)

	answer := make(chan error, 1)
	pump.work = make(chan nativePumpWork, 1)
	pump.work <- nativePumpWork{barrier: answer}
	pump.drain(t.Context())
	require.NoError(t, <-answer)
}

func TestNativePumpBarrierAfterOutboxExit(t *testing.T) {
	session := &agentSession{agent: NewAgent()}

	retired := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		workDone: make(chan struct{}),
	}
	close(retired.workDone)
	require.NoError(t, retired.barrier(t.Context()))

	stopped := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	go func() {
		<-stopped.work
		close(stopped.workDone)
	}()
	require.NoError(t, stopped.barrier(t.Context()))

	cancelled := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-cancelled.work
		cancel()
	}()
	require.ErrorIs(t, cancelled.barrier(ctx), context.Canceled)
}

func TestServeNativePumpPropagatesIncarnationEndFailure(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, session.serveNativePump(ctx, claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())))

	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	_, err = stream.announceAction(ctx, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)

	conn.sessionUpdateErr = errors.New("loss delivery")
	err = session.serveNativePump(ctx, claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()))
	require.ErrorContains(t, err, "loss delivery")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Nil(t, pump.client, "the replacement is never published when the old incarnation cannot retire")
	pump.mu.Unlock()

	session.stopNativePump()
}
