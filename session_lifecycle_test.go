package claudeacp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionLifecycleBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	session.turn = make(chan struct{}, 1)
	session.turn <- struct{}{}
	release, err := session.acquireTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)

	// A second prompt to a busy session (its single turn slot taken) is refused
	// with session_prompt backpressure; per-session turn capacity is fixed at 1.
	session.turn = make(chan struct{}, sessionTurnCapacity)
	session.turn <- struct{}{}
	release, err = session.acquireTurn(ctx)
	requireBackpressureLimit(t, err, "session_prompt")
	require.Nil(t, release)

	session.turn = make(chan struct{}, 2)
	release, err = session.acquireExclusiveTurn(ctx)
	require.NoError(t, err)
	require.Len(t, session.turn, 2)
	release()
	require.Empty(t, session.turn)

	partialCtx, partialCancel := context.WithCancel(ctx)
	session.turn = make(chan struct{}, 2)
	session.turnAcquiredHook = func(acquired int) {
		if acquired == 1 {
			partialCancel()
			session.turn <- struct{}{}
		}
	}
	release, err = session.acquireExclusiveTurn(partialCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	<-session.turn
	require.Empty(t, session.turn)
	session.turnAcquiredHook = nil

	innerCancelCtx := &nthDoneContext{
		done:       make(chan struct{}),
		closeAfter: 3,
	}
	session.turn = make(chan struct{}, 2)
	session.turnAcquiredHook = func(acquired int) {
		if acquired == 1 {
			session.turn <- struct{}{}
		}
	}
	release, err = session.acquireExclusiveTurn(innerCancelCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	<-session.turn
	require.Empty(t, session.turn)
	session.turnAcquiredHook = nil

	session.turn = make(chan struct{})
	release, err = session.acquireExclusiveTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)

	session.turn = nil
	require.NotNil(t, session.turnQueue())

	permissionCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"p1": {cancel: func() { permissionCancelled = true }},
	}
	cancels := session.cancelPermissionRequestsLocked()
	require.Len(t, cancels, 1)
	cancels[0]()
	require.True(t, permissionCancelled)
	require.Empty(t, session.permissionCancel)

	closeSession, closeCleanup := newStartedAgentSessionForTest(t, agent, "close")
	defer closeCleanup()
	closeSession.turn = make(chan struct{}, 1)
	closeSession.turn <- struct{}{}
	closeSession.closeTurnWait = time.Millisecond
	err = closeSession.Close(ctx)
	require.Error(t, err)
}

func TestSessionCancelAndCloseEdgeBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "cancel")
	defer cleanup()

	permissionCancelled := false
	turnCancelled := false
	session.cancel = func() { turnCancelled = true }
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	require.NoError(t, session.Cancel(ctx))
	require.True(t, permissionCancelled)
	require.True(t, session.turnCancelled)
	// Cancel cancels the local turn context synchronously, before the native
	// interrupt resolves.
	require.True(t, turnCancelled)

	notStarted := &agentSession{client: claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())}
	require.ErrorIs(t, notStarted.Cancel(ctx), claude.ErrClientNotStarted)

	closeSession, closeCleanup := newStartedAgentSessionForTest(t, agent, "close-edge")
	defer closeCleanup()
	cancelled := false
	closeSession.cancel = func() { cancelled = true }
	closeSession.materialized = &materializedSession{configDir: string([]byte{0})}
	err := closeSession.Close(ctx)
	require.Error(t, err)
	require.True(t, cancelled)

	closeErrSession, closeErrCleanup := newStartedAgentSessionForTest(t, agent, "close-error")
	defer closeErrCleanup()
	closeErrSession.client = claude.NewClient(nil, claude.Options{}, &closeErrTransport{Transport: newFakeClaudeTransport(), err: errors.New("close failed")})
	require.NoError(t, closeErrSession.client.Start(ctx))
	err = closeErrSession.Close(ctx)
	require.ErrorContains(t, err, "close failed")
}

func TestSessionCancelInterruptDetachedFromCallerContext(t *testing.T) {
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "detached-interrupt")
	defer cleanup()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// The native interrupt runs under a bounded background-derived context, so a
	// cancelled caller context does not abort it.
	require.NoError(t, session.Cancel(cancelledCtx))
}

func TestSessionCancelCancelsPendingElicitations(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "elicit-cancel")
	defer cleanup()

	permissionCancelled := false
	elicitationCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	session.elicitationCancel = map[int64]*elicitationRequestCancel{
		1: {cancel: func() { elicitationCancelled = true }},
	}

	require.NoError(t, session.Cancel(ctx))
	require.True(t, permissionCancelled)
	require.True(t, elicitationCancelled)
	require.True(t, session.turnCancelled)
	require.Empty(t, session.permissionCancel)
	require.Empty(t, session.elicitationCancel)
}

func TestSessionCloseResolvesPendingInteractionsFirst(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "close-pending")
	defer cleanup()

	permissionCancelled := false
	elicitationCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	session.elicitationCancel = map[int64]*elicitationRequestCancel{
		1: {cancel: func() { elicitationCancelled = true }},
	}

	require.NoError(t, session.Close(ctx))
	require.True(t, permissionCancelled)
	require.True(t, elicitationCancelled)
	require.True(t, session.turnCancelled)
	require.Empty(t, session.permissionCancel)
	require.Empty(t, session.elicitationCancel)
}

func TestRegisterElicitationCancellation(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "elicit-register")
	defer cleanup()

	elicitationCtx, finish := session.registerElicitation(ctx)
	require.NoError(t, elicitationCtx.Err())
	require.Len(t, session.elicitationCancel, 1)

	require.NoError(t, session.Cancel(ctx))
	require.Error(t, elicitationCtx.Err())
	require.Empty(t, session.elicitationCancel)

	finish()

	// A registration made after the turn is already cancelled is cancelled at once.
	preCancelledCtx, preFinish := session.registerElicitation(ctx)
	defer preFinish()
	require.Error(t, preCancelledCtx.Err())
}

type nthDoneContext struct {
	done       chan struct{}
	closeAfter int
	calls      int
	once       sync.Once
}

func (c *nthDoneContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c *nthDoneContext) Done() <-chan struct{} {
	c.calls++
	if c.calls >= c.closeAfter {
		c.once.Do(func() { close(c.done) })
	}

	return c.done
}

func (c *nthDoneContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *nthDoneContext) Value(any) any {
	return nil
}
