package claudeacp

import (
	"context"
	"errors"
	"time"
)

var sessionCancelFallbackTimeout = 5 * time.Second

func (s *Session) acquireTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()

	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) sessionWorkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	turnDone := s.turnDone
	s.mu.Unlock()

	if turnDone == nil {
		return ctx, func() {}
	}

	select {
	case <-turnDone:
		workCtx, cancel := context.WithCancel(ctx)
		cancel()

		return workCtx, func() {}
	default:
	}

	workCtx, cancel := context.WithCancel(ctx)

	go func() {
		defer recoverAgentGoroutine(ctx, nil, "session work watcher")

		// Tie auxiliary work to the active turn. The watcher exits when the turn
		// completes or when the caller invokes the returned cancel function.
		select {
		case <-turnDone:
			cancel()
		case <-workCtx.Done():
		}
	}()

	return workCtx, cancel
}

func (s *Session) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = make(chan struct{}, 1)
	}

	return s.turn
}

// Cancel interrupts the active Claude turn.
func (s *Session) Cancel(ctx context.Context) (err error) {
	s.mu.Lock()
	turnCancel := s.cancel
	turnDone := s.turnDone

	if s.cancel != nil || len(s.permissionCancel) > 0 {
		s.turnCancelled = true
	}

	permissionCancels := s.cancelPermissionRequestsLocked()
	s.mu.Unlock()

	for _, cancel := range permissionCancels {
		cancel()
	}

	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "interrupt")
		defer func() { finish(err) }()
	}

	err = s.client.Interrupt(ctx)
	s.cancelTurnIfInterruptStalls(ctx, turnDone, turnCancel)

	return err
}

func (s *Session) cancelTurnIfInterruptStalls(ctx context.Context, done <-chan struct{}, cancel context.CancelFunc) {
	if cancel == nil || done == nil || sessionCancelFallbackTimeout <= 0 {
		return
	}

	go func() {
		defer recoverAgentGoroutine(ctx, nil, "Claude interrupt fallback")

		timer := time.NewTimer(sessionCancelFallbackTimeout)
		defer timer.Stop()

		select {
		case <-done:
		case <-timer.C:
			if s.agent != nil {
				s.agent.log.DebugContext(context.WithoutCancel(ctx), "cancel Claude turn after interrupt timeout")
			}

			cancel()
		}
	}()
}

func (s *Session) cancelPermissionRequestsLocked() []context.CancelFunc {
	permissionCancels := make([]context.CancelFunc, 0, len(s.permissionCancel))
	for id, entry := range s.permissionCancel {
		permissionCancels = append(permissionCancels, entry.cancel)

		delete(s.permissionCancel, id)
	}

	return permissionCancels
}

// Close closes the underlying Claude process.
func (s *Session) Close(ctx context.Context) (err error) {
	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "close")
		defer func() { finish(err) }()
	}

	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	err = s.client.Close()

	waitCtx, stopWaiting := context.WithTimeout(ctx, s.closeTurnTimeout())
	defer stopWaiting()

	if releaseTurn, waitErr := s.acquireTurn(waitCtx); waitErr != nil {
		err = errors.Join(err, waitErr)
	} else {
		releaseTurn()
	}

	if s.mcpBridge != nil {
		err = errors.Join(err, s.mcpBridge.Close())
	}

	if s.materialized != nil {
		err = errors.Join(err, s.materialized.Close())
	}

	if s.agent != nil {
		s.agent.observe.RecordClaudeProcessExit(ctx, "closed", err)
	}

	return err
}

func (s *Session) closeTurnTimeout() time.Duration {
	if s.closeTurnWait > 0 {
		return s.closeTurnWait
	}

	return defaultSessionCloseTurnWait
}
