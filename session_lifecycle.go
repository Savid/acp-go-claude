package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

// sessionInterruptTimeout bounds the native interrupt. The interrupt runs under
// a background-derived context so a cancelled caller context cannot abort it.
var sessionInterruptTimeout = 5 * time.Second
var sessionRemoveAll = os.RemoveAll

// finalizeSessionRuntimeResources returns admissions only after the resource
// they account for is gone. A failed process-tree proof retains both the
// native admission and every adapter-owned scratch root because that tree may
// still be using them. Other close errors do not obscure a successful proof.
func finalizeSessionRuntimeResources(
	runtimeErr error,
	nativeRelease func(),
	mcpConfigDir string,
	materialized *materializedSession,
	scratchRelease func(),
) error {
	if errors.Is(runtimeErr, claude.ErrProcessTreeUnproven) {
		return runtimeErr
	}

	if nativeRelease != nil {
		nativeRelease()
	}

	var mcpRemoveErr error
	if mcpConfigDir != "" {
		mcpRemoveErr = sessionRemoveAll(mcpConfigDir)
	}

	var materializedRemoveErr error
	if materialized != nil {
		materializedRemoveErr = materialized.Close()
	}

	if mcpRemoveErr == nil && materializedRemoveErr == nil && scratchRelease != nil {
		scratchRelease()
	}

	return errors.Join(runtimeErr, mcpRemoveErr, materializedRemoveErr)
}

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
		s.turn = make(chan struct{}, sessionTurnCapacity)
	}

	return s.turn
}

// ensureClientAlive relaunches the native Claude process when it died on a
// previous turn. The session is never removed on a native failure, so a
// follow-up prompt lands here and brings the process back up (resuming the
// native session id) rather than returning the unknown-session error. It is a
// no-op for sessions that cannot relaunch (e.g. injected test clients) and for
// clients that are still alive.
func (s *agentSession) ensureClientAlive(ctx context.Context) error {
	s.mu.Lock()
	canRelaunch := s.canRelaunch
	client := s.client
	nativeRelease := s.nativeRootRelease
	s.mu.Unlock()

	if !canRelaunch {
		return nil
	}

	if client != nil && client.Alive() {
		return nil
	}

	opts := s.clientOptions
	opts.ResumeID = string(s.id)

	var previousCloseErr error
	if client != nil {
		previousCloseErr = client.Close()
		if errors.Is(previousCloseErr, claude.ErrProcessTreeUnproven) {
			s.mu.Lock()
			s.canRelaunch = false
			s.mu.Unlock()

			return fmt.Errorf("prove previous Claude process tree quiescent: %w", previousCloseErr)
		}
	}

	if nativeRelease != nil {
		nativeRelease()
	}

	s.mu.Lock()
	if s.client == client {
		s.nativeRootRelease = nil
	}
	s.mu.Unlock()

	nativeRelease, err := acquireNativeRoot(ctx, s.agent.options.RuntimeResourceHooks, RuntimeResourceSession)
	if err != nil {
		return err
	}

	relaunched := s.agent.newClaudeClient(s.agent.log, opts)
	if err := relaunched.Start(ctx); err != nil {
		cleanupErr := errors.Join(err, relaunched.Close())

		if !errors.Is(cleanupErr, claude.ErrProcessTreeUnproven) {
			nativeRelease()
		} else {
			// No later path may launch another root under an admission whose
			// process-tree quiescence could not be proven.
			s.mu.Lock()
			s.canRelaunch = false
			s.mu.Unlock()
		}

		return errors.Join(previousCloseErr, cleanupErr)
	}

	s.mu.Lock()
	s.client = relaunched
	s.nativeRootRelease = nativeRelease
	s.mu.Unlock()

	return previousCloseErr
}

// Cancel cancels the active Claude turn. The local turn context is cancelled
// synchronously so the prompt resolves cancelled deterministically; the native
// interrupt then runs on its own bounded background context so a cancelled
// caller context cannot abort it.
func (s *agentSession) Cancel(ctx context.Context) (err error) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()

	return s.cancelNative(ctx)
}

// cancelRouted validates the active turn and keeps its native interrupt fenced
// from turn completion and admission of the next turn.
func (s *agentSession) cancelRouted(ctx context.Context, meta map[string]any) error {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()

	s.mu.Lock()
	activeNonce := s.turnNonce
	active := s.cancel != nil && activeNonce != ""
	s.mu.Unlock()

	if active {
		route, err := parseInboundTurnRoute(meta)
		if err != nil {
			return err
		}

		if route.turnNonce != activeNonce {
			return routeInvalid("stale route turnNonce")
		}
	}

	return s.cancelNative(ctx)
}

func (s *agentSession) cancelNative(ctx context.Context) (err error) {
	s.mu.Lock()
	turnCancel := s.cancel
	s.mu.Unlock()

	s.cancelPendingInteractions()

	if turnCancel != nil {
		turnCancel()
	}

	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "interrupt")
		defer func() { finish(err) }()
	}

	interruptCtx, cancelInterrupt := context.WithTimeout(context.WithoutCancel(ctx), sessionInterruptTimeout)
	defer cancelInterrupt()

	return s.client.Interrupt(interruptCtx)
}

func (s *agentSession) currentTurnNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnNonce
}

// cancelPendingInteractions marks the turn cancelled and resolves any pending
// permission and elicitation requests as cancelled. Callers invoke this before
// native abort so outstanding client requests are answered cancelled first.
func (s *agentSession) cancelPendingInteractions() {
	s.mu.Lock()
	if s.cancel != nil || len(s.permissionCancel) > 0 || len(s.elicitationCancel) > 0 {
		s.turnCancelled = true
	}

	permissionCancels := s.cancelPermissionRequestsLocked()
	elicitationCancels := s.cancelElicitationRequestsLocked()
	s.mu.Unlock()

	for _, cancel := range permissionCancels {
		cancel()
	}

	for _, cancel := range elicitationCancels {
		cancel()
	}
}

func (s *agentSession) cancelPermissionRequestsLocked() []context.CancelFunc {
	permissionCancels := make([]context.CancelFunc, 0, len(s.permissionCancel))
	for id, entry := range s.permissionCancel {
		permissionCancels = append(permissionCancels, entry.cancel)

		delete(s.permissionCancel, id)
	}

	return permissionCancels
}

func (s *agentSession) cancelElicitationRequestsLocked() []context.CancelFunc {
	elicitationCancels := make([]context.CancelFunc, 0, len(s.elicitationCancel))
	for id, entry := range s.elicitationCancel {
		elicitationCancels = append(elicitationCancels, entry.cancel)

		delete(s.elicitationCancel, id)
	}

	return elicitationCancels
}

// Close closes the underlying Claude process.
func (s *agentSession) Close(ctx context.Context) (err error) {
	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "close")
		defer func() { finish(err) }()
	}

	s.cancelPendingInteractions()

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

	err = finalizeSessionRuntimeResources(
		err,
		s.nativeRootRelease,
		s.mcpConfigDir,
		s.materialized,
		s.scratchRootRelease,
	)

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
