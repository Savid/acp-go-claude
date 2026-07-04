package claudeacp

import (
	"context"
	"errors"
	"time"
)

var sessionCancelFallbackTimeout = 5 * time.Second

func (s *agentSession) acquireTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()

	select {
	case turn <- struct{}{}:
		s.afterTurnSlotAcquired(1)

		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, backpressureError("session_prompt")
	}
}

func (s *agentSession) acquireExclusiveTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()

	capacity := cap(turn)
	if capacity <= 0 {
		capacity = 1
	}

	acquired := 0
	for acquired < capacity {
		select {
		case turn <- struct{}{}:
			acquired++
			s.afterTurnSlotAcquired(acquired)
		case <-ctx.Done():
			for ; acquired > 0; acquired-- {
				<-turn
			}

			return nil, ctx.Err()
		default:
			select {
			case <-ctx.Done():
				for ; acquired > 0; acquired-- {
					<-turn
				}

				return nil, ctx.Err()
			default:
			}

			for ; acquired > 0; acquired-- {
				<-turn
			}

			return nil, backpressureError("session_prompt")
		}
	}

	return func() {
		for ; acquired > 0; acquired-- {
			<-turn
		}
	}, nil
}

func (s *agentSession) afterTurnSlotAcquired(acquired int) {
	if s.turnAcquiredHook != nil {
		s.turnAcquiredHook(acquired)
	}
}

func (s *agentSession) turnQueue() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.turn == nil {
		s.turn = make(chan struct{}, s.maxConcurrentPrompts())
	}

	return s.turn
}

// Cancel interrupts the active Claude turn.
func (s *agentSession) Cancel(ctx context.Context) (err error) {
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

func (s *agentSession) cancelTurnIfInterruptStalls(ctx context.Context, done <-chan struct{}, cancel context.CancelFunc) {
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

func (s *agentSession) cancelPermissionRequestsLocked() []context.CancelFunc {
	permissionCancels := make([]context.CancelFunc, 0, len(s.permissionCancel))
	for id, entry := range s.permissionCancel {
		permissionCancels = append(permissionCancels, entry.cancel)

		delete(s.permissionCancel, id)
	}

	return permissionCancels
}

// Close closes the underlying Claude process.
func (s *agentSession) Close(ctx context.Context) (err error) {
	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "close")
		defer func() { finish(err) }()
	}

	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()

	s.stopLateMirrorProcessor(ctx)

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

	if s.materialized != nil {
		err = errors.Join(err, s.materialized.Close())
	}

	if s.agent != nil {
		s.agent.observe.RecordClaudeProcessExit(ctx, "closed", err)
	}

	return err
}

func (s *agentSession) closeTurnTimeout() time.Duration {
	if s.closeTurnWait > 0 {
		return s.closeTurnWait
	}

	return defaultSessionCloseTurnWait
}
