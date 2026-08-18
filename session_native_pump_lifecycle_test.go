package claudeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
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
