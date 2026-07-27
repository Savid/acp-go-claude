package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// sessionInterruptTimeout bounds the native interrupt. The interrupt runs under
// a background-derived context so a cancelled caller context cannot abort it.
var sessionInterruptTimeout = 5 * time.Second
var sessionRemoveAll = os.RemoveAll

// finalizeSessionRuntimeResources returns admissions only after the selected
// containment boundary completes. An incomplete boundary retains both the
// native admission and every adapter-owned scratch root because escaped work
// may still be using them. Other close errors do not obscure completion.
func finalizeSessionRuntimeResources(
	runtimeErr error,
	nativeRelease func(),
	mcpConfigDir string,
	imageScratchDir string,
	materialized *materializedSession,
	scratchRelease func(),
) error {
	if errors.Is(runtimeErr, claude.ErrProcessContainmentIncomplete) {
		return runtimeErr
	}

	if nativeRelease != nil {
		nativeRelease()
	}

	var mcpRemoveErr error
	if mcpConfigDir != "" {
		mcpRemoveErr = sessionRemoveAll(mcpConfigDir)
	}

	var imageRemoveErr error
	if imageScratchDir != "" {
		imageRemoveErr = sessionRemoveAll(imageScratchDir)
	}

	var materializedRemoveErr error
	if materialized != nil {
		materializedRemoveErr = materialized.Close()
	}

	if mcpRemoveErr == nil && imageRemoveErr == nil && materializedRemoveErr == nil && scratchRelease != nil {
		scratchRelease()
	}

	return errors.Join(runtimeErr, mcpRemoveErr, imageRemoveErr, materializedRemoveErr)
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
	opts.ForkSession = false

	return s.relaunchClient(ctx, client, nativeRelease, opts)
}

// refreshMCPRegistry rebuilds Claude's fixed MCP tool registry exactly once,
// after the host has armed the first user turn and before the model sees that
// turn. Session establishment may intentionally observe only a provisional
// runtime_ready surface; the session's private MCP descriptor itself remains
// unchanged and is reused by the replacement process.
func (s *agentSession) refreshMCPRegistry(ctx context.Context) error {
	s.mu.Lock()
	pending := s.mcpRefreshPending
	canRelaunch := s.canRelaunch
	client := s.client
	nativeRelease := s.nativeRootRelease
	opts := s.clientOptions
	s.mu.Unlock()

	if !pending {
		return nil
	}

	if !canRelaunch {
		// Injected unit-test sessions have no process launch contract. Their
		// transport already represents the effective tool registry.
		return nil
	}

	if err := s.relaunchClient(ctx, client, nativeRelease, opts); err != nil {
		return err
	}

	s.mu.Lock()
	s.mcpRefreshPending = false
	s.mu.Unlock()

	return nil
}

type relaunchConfig struct {
	model          string
	modelOverrides map[string]string
	outputStyle    string
	effort         string
	mode           acp.SessionModeId
}

func (s *agentSession) currentRelaunchConfig() relaunchConfig {
	s.mu.Lock()
	defer s.mu.Unlock()

	return relaunchConfig{
		model:          s.model,
		modelOverrides: cloneStringMap(s.modelOverrides),
		outputStyle:    s.outputStyle,
		effort:         s.effort,
		mode:           s.mode,
	}
}

// relaunchClient replaces one completed native containment boundary without
// ever holding two session native-root admissions at once. A failed or
// cancelled launch is fully closed before its new admission is returned; an
// incomplete boundary poisons relaunch permanently and retains the admission.
func (s *agentSession) relaunchClient(
	ctx context.Context,
	client *claude.Client,
	nativeRelease func(),
	opts claude.Options,
) (returnErr error) {
	if err := s.agent.beginSessionConstruction(); err != nil {
		return err
	}
	defer func() {
		s.recordContainmentError(returnErr)
		s.agent.endSessionConstruction()
	}()

	config := s.currentRelaunchConfig()
	if config.model != "" {
		opts.Model = claudeModelID(config.model, config.modelOverrides)
	}

	if permissionMode, ok := permissionModeForACP(config.mode); ok {
		opts.PermissionMode = permissionMode
	}

	var previousCloseErr error
	if client != nil {
		previousCloseErr = client.Close()
		if errors.Is(previousCloseErr, claude.ErrProcessContainmentIncomplete) {
			s.mu.Lock()
			s.canRelaunch = false
			s.mu.Unlock()
			s.recordContainmentError(previousCloseErr)

			return fmt.Errorf("complete previous Claude containment boundary: %w", previousCloseErr)
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
		return s.cleanupFailedRelaunch(err, relaunched, nativeRelease, previousCloseErr)
	}

	if config.outputStyle != "" {
		if err := relaunched.SetOutputStyle(ctx, config.outputStyle); err != nil {
			return s.cleanupFailedRelaunch(err, relaunched, nativeRelease, previousCloseErr)
		}
	}

	if config.effort != "" {
		if err := relaunched.SetEffort(ctx, config.effort); err != nil {
			return s.cleanupFailedRelaunch(err, relaunched, nativeRelease, previousCloseErr)
		}
	}

	s.mu.Lock()
	s.client = relaunched
	s.nativeRootRelease = nativeRelease
	s.mu.Unlock()

	return previousCloseErr
}

func (s *agentSession) cleanupFailedRelaunch(
	cause error,
	relaunched *claude.Client,
	nativeRelease func(),
	previousCloseErr error,
) error {
	cleanupErr := errors.Join(cause, relaunched.Close())

	if !errors.Is(cleanupErr, claude.ErrProcessContainmentIncomplete) {
		if nativeRelease != nil {
			nativeRelease()
		}
	} else {
		// No later path may launch another root under an admission whose
		// selected containment boundary did not complete.
		s.mu.Lock()
		s.canRelaunch = false
		s.mu.Unlock()
		s.recordContainmentError(cleanupErr)
	}

	return errors.Join(previousCloseErr, cleanupErr)
}

// Cancel cancels the active Claude turn. Settlement is fenced by cancelMu: the
// local turn wakes immediately, but neither Cancel nor Prompt may return until
// the selected native containment boundary has completed. A later prompt
// lazily relaunches Claude and resumes the same native session id.
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
	client := s.client
	active := turnCancel != nil || len(s.permissionCancel) > 0 || len(s.elicitationCancel) > 0
	s.mu.Unlock()

	s.cancelPendingInteractions(true)

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

	var interruptErr error
	if client == nil {
		interruptErr = claude.ErrClientNotStarted
	} else {
		interruptErr = client.Interrupt(interruptCtx)
	}

	var closeErr error
	if active {
		closeErr = s.closeNativeClient(client)
	}

	s.mu.Lock()
	if errors.Is(closeErr, claude.ErrProcessContainmentIncomplete) {
		s.turnContainmentErr = closeErr
	} else {
		s.turnContainmentErr = nil
	}
	s.mu.Unlock()

	return errors.Join(interruptErr, closeErr)
}

// closeNativeClient terminates the selected native containment boundary and
// returns its admission only after that boundary completes. An incomplete
// boundary permanently disables relaunch and retains its admission.
func (s *agentSession) closeNativeClient(client *claude.Client) error {
	if client == nil {
		return nil
	}

	closeErr := client.Close()
	if errors.Is(closeErr, claude.ErrProcessContainmentIncomplete) {
		s.mu.Lock()
		s.canRelaunch = false
		s.mu.Unlock()
		s.recordContainmentError(closeErr)

		return closeErr
	}

	var nativeRelease func()

	s.mu.Lock()
	if s.client == client {
		nativeRelease = s.nativeRootRelease
		s.nativeRootRelease = nil
	}
	s.mu.Unlock()

	if nativeRelease != nil {
		nativeRelease()
	}

	return closeErr
}

func (s *agentSession) currentTurnNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnNonce
}

// cancelPendingInteractions marks the turn cancelled and resolves any pending
// permission and elicitation requests as cancelled. Callers invoke this before
// native abort so outstanding client requests are answered cancelled first.
func (s *agentSession) cancelPendingInteractions(markTurnCancelled bool) {
	s.mu.Lock()
	if markTurnCancelled && (s.cancel != nil || len(s.permissionCancel) > 0 || len(s.elicitationCancel) > 0) {
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

// Close closes the underlying Claude process and memoizes the terminal result.
func (s *agentSession) Close(ctx context.Context) (err error) {
	s.closeOnce.Do(func() {
		s.closeErr = s.close(ctx)
	})

	return s.closeErr
}

func (s *agentSession) close(ctx context.Context) (err error) {
	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "close")
		defer func() { finish(err) }()
	}

	s.cancelPendingInteractions(true)

	// Pending provider-auth flows are cancelled after pending input is resolved
	// and before the native interrupt, so a flow is never abandoned to a
	// process that is already being torn down.
	if s.agent != nil {
		s.agent.providerAuth.closeSession(s.id)
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

	err = finalizeSessionRuntimeResources(
		err,
		s.nativeRootRelease,
		s.mcpConfigDir,
		s.imageScratchDir,
		s.materialized,
		s.scratchRootRelease,
	)

	if s.agent != nil {
		s.agent.observe.RecordClaudeProcessExit(ctx, "closed", err)
		s.agent.recordContainmentError(err)
	}

	return err
}

func (s *agentSession) recordContainmentError(err error) {
	if s.agent != nil {
		s.agent.recordContainmentError(err)
	}
}

func (s *agentSession) closeTurnTimeout() time.Duration {
	if s.closeTurnWait > 0 {
		return s.closeTurnWait
	}

	return defaultSessionCloseTurnWait
}
