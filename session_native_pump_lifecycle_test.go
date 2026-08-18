package claudeacp

import (
	"context"
	"errors"
	"sync"
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

// TestServeNativePumpContainsAnIncarnationItCannotAnnounce proves the opening
// snapshot precedes the reader: a snapshot the host never received leaves no
// reader running, no pump state published, and no live process the host was never
// told about.
func TestServeNativePumpContainsAnIncarnationItCannotAnnounce(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(ctx))
	session.client = client

	conn.sessionUpdateErr = errors.New("snapshot delivery")
	require.ErrorContains(t, session.serveNativePump(ctx, client), "snapshot delivery")
	require.Equal(t, 1, transport.CloseCalls(), "the process the snapshot could not name is contained")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	defer pump.mu.Unlock()
	require.Nil(t, pump.client)
	require.Nil(t, pump.stop)
	require.Nil(t, pump.done, "no reader was ever started")
}

// TestServeNativePumpAdmitsOneIncarnationUnderConcurrentCallers proves the whole
// transition is one step. Session establishment and a prompt both point the pump
// at the same process, and exactly one incarnation is announced and one reader
// serves it — never two identities for one process lifetime.
func TestServeNativePumpAdmitsOneIncarnationUnderConcurrentCallers(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)
	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())

	const callers = 8

	var (
		serves  sync.WaitGroup
		results = make([]error, callers)
	)

	serves.Add(callers)

	for index := range callers {
		go func() {
			defer serves.Done()

			results[index] = session.serveNativePump(ctx, client)
		}()
	}

	serves.Wait()

	for _, err := range results {
		require.NoError(t, err)
	}

	snapshots := 0

	for _, eventType := range lifecycleEventTypes(t, conn) {
		if eventType == string(lifecycle.EventSnapshot) {
			snapshots++
		}
	}

	require.Equal(t, 1, snapshots, "one process lifetime is one incarnation")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Same(t, client, pump.client)
	pump.mu.Unlock()

	session.stopNativePump()
}

// TestServeNativePumpRefusesAClosingSession proves close is terminal for the pump
// too: the post-response establishment hook can land while a close is tearing the
// session down, and it starts no reader and opens no incarnation behind it.
func TestServeNativePumpRefusesAClosingSession(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)
	session.beginClose()

	err := session.serveNativePump(ctx, claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()))
	require.Error(t, err)

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Nil(t, pump.client)
	pump.mu.Unlock()

	require.Empty(t, lifecycleEventTypes(t, conn), "a closing session announces no incarnation")
}
