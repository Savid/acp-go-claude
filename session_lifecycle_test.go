package claudeacp

import (
	"context"
	"errors"
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

	session.turn = make(chan struct{}, 1)
	session.turn <- struct{}{}
	release, err = session.acquireTurn(ctx)
	require.Error(t, err)
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

	previousTimeout := sessionCancelFallbackTimeout
	sessionCancelFallbackTimeout = time.Millisecond
	t.Cleanup(func() { sessionCancelFallbackTimeout = previousTimeout })
	done := make(chan struct{})
	fallbackCancelled := make(chan struct{})
	session.cancelTurnIfInterruptStalls(ctx, done, func() { close(fallbackCancelled) })
	select {
	case <-fallbackCancelled:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fallback cancel did not fire")
	}
	close(done)

	sessionCancelFallbackTimeout = 0
	session.cancelTurnIfInterruptStalls(ctx, make(chan struct{}), func() { t.Fatal("unexpected fallback cancel") })

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
	done := make(chan struct{})
	close(done)
	session.cancel = func() {}
	session.turnDone = done
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	require.NoError(t, session.Cancel(ctx))
	require.True(t, permissionCancelled)
	require.True(t, session.turnCancelled)

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
