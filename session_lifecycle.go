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

// errSessionCloseUnsettled marks a close its settlement barrier never admitted.
// Such a close tore nothing down and settled nothing, so the id keeps its live
// session and a later caller takes the barrier again.
var errSessionCloseUnsettled = errors.New("await the in-flight Claude turn")

// sessionCloseUnsettledError is the wire answer for that close. Nothing failed
// internally: the boundary was never reached, and the truthful report is that
// this close settled nothing and may be taken again.
//
// Which answer that is depends on why the barrier wait ended. A wait the caller
// cancelled is the caller's own $/cancel_request coming back, so the raw error
// travels undressed and the dispatcher answers the one code a withdrawn request
// has: -32800. Any other expiry is a retryable refusal of a well-formed request,
// which is the family's invalid-request idiom, and its message carries the
// barrier-wait error rather than anything the native process said.
func sessionCloseUnsettledError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}

	return acp.NewInvalidRequest(map[string]any{
		jsonFieldError:   "claude_session_close_unsettled",
		jsonFieldMessage: err.Error(),
	})
}

// sessionDeleteUnsettledError is the wire answer for a delete whose teardown
// never took the session's settlement barrier. That is the same unreached
// boundary session/close reports, so it is answered the same way and for the
// same reason: nothing failed internally, the teardown simply never ran, and the
// next delete takes the barrier again. A caller that withdrew its own request
// gets the raw error and the -32800 that goes with it; any other expiry is the
// family's invalid-request idiom, naming itself.
//
// It names itself apart from close because the two refusals do not report the
// same state. A refused close settled nothing at all and its id still names a
// live session; a refused delete already wrote its durable tombstone and already
// hid the id, so the deletion the host asked for has happened and only the
// teardown behind it is still owed.
//
// Anything else the teardown reports travels unchanged. A containment or cleanup
// failure is a real internal failure and keeps the answer it earned.
func sessionDeleteUnsettledError(err error) error {
	if !errors.Is(err, errSessionCloseUnsettled) {
		return err
	}

	if errors.Is(err, context.Canceled) {
		return err
	}

	return acp.NewInvalidRequest(map[string]any{
		jsonFieldError:   "claude_session_delete_unsettled",
		jsonFieldMessage: err.Error(),
	})
}

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

// acquireClosingTurn admits close into the session's turn queue. It is the one
// admission with no fail-fast arm: a prompt that finds the queue full can be
// answered with backpressure and retried, while close is about to tear the
// native process out from under whatever holds the slot and therefore has to
// wait for it. The caller's context bounds the wait.
//
// A free slot is taken before the caller's context is consulted at all, so an
// expired caller loses the barrier only to a turn that really holds it. Offering
// both to one select would let an already-cancelled close of a quiescent session
// report an in-flight turn that does not exist.
func (s *agentSession) acquireClosingTurn(ctx context.Context) (func(), error) {
	turn := s.turnQueue()

	select {
	case turn <- struct{}{}:
		s.afterTurnSlotAcquired(1)

		return func() { <-turn }, nil
	default:
	}

	select {
	case turn <- struct{}{}:
		s.afterTurnSlotAcquired(1)

		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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
	closing := s.closing
	client := s.client
	nativeRelease := s.nativeRootRelease
	opts := s.clientOptions
	s.mu.Unlock()

	if !pending {
		return nil
	}

	if closing {
		return closedSessionError()
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
	if s.isClosing() {
		return closedSessionError()
	}

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
	if s.closing {
		s.mu.Unlock()

		// A relaunch that lost the race to close must not leave a native process
		// behind it: the replacement is torn down and its admission returned
		// before the refusal is answered.
		return s.cleanupFailedRelaunch(closedSessionError(), relaunched, nativeRelease, previousCloseErr)
	}

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
// from turn completion and admission of the next turn. The lifecycle key never
// rides session/cancel: it fails the cancel closed before any native interrupt,
// and the cancel is never applied. Being a notification, the refusal is
// wire-silent.
//
// The route is validated first. This surface carries both reserved objects, and
// the route is the authenticator: it decides whether the caller is addressing
// the turn that is actually running, which precedes the placement rule about
// where a family literal may ride. A cancel carrying both an invalid route and
// the lifecycle key therefore reports the route, and never one of two verdicts
// chosen by whichever check an implementation happened to run first.
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
			return unsupportedField(routeMetaKey)
		}
	}

	if err := rejectLifecycleMeta(meta); err != nil {
		return err
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
// A second caller blocks until the first finishes and observes that same
// result; the teardown, the native signalling and the active-session accounting
// behind that result all run exactly once.
//
// A close its settlement barrier never admitted is the one result that is not
// memoized. Nothing was torn down and nothing was settled, so there is no
// terminal result to hand a later caller, and that caller takes the barrier
// again under a context of its own.
func (s *agentSession) Close(ctx context.Context) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closeSettled {
		return s.closeErr
	}

	err := s.close(ctx)
	if errors.Is(err, errSessionCloseUnsettled) {
		return err
	}

	s.closeSettled = true
	s.closeErr = err

	return err
}

// beginClose latches the terminal close state before any teardown runs, so a
// prompt, relaunch, registry refresh or reuse that is deciding whether to start
// native work sees the close that is about to remove the process it would use.
func (s *agentSession) beginClose() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
}

// isClosing reports the terminal close state.
func (s *agentSession) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closing
}

// closedSessionError is the answer every door gives once close has begun. The
// id is on its way out of the active map, so a caller that raced the close is
// told what it would be told a moment later.
func closedSessionError() error {
	return unknownSessionError()
}

// awaitSettledTurn is the close barrier, and it is the full-settlement latch. A
// turn releases the session's turn slot only after its own settlement has
// finished — its containment boundary, its durable commit, and its terminal
// lifecycle event — so acquiring that slot is what proves there is nothing left
// to settle. The caller's context is the only bound: a deadline here would tear
// the process out from under a commit that is still landing, which is the exact
// thing waiting for the turn prevents.
func (s *agentSession) awaitSettledTurn(ctx context.Context) (func(), error) {
	releaseTurn, err := s.acquireClosingTurn(ctx)
	if err != nil {
		return func() {}, fmt.Errorf("%w: %w", errSessionCloseUnsettled, err)
	}

	return releaseTurn, nil
}

func (s *agentSession) close(ctx context.Context) (err error) {
	if s.agent != nil {
		var finish func(error)

		ctx, finish = s.agent.observe.StartClaudeProcess(ctx, "close")
		defer func() { finish(err) }()
	}

	s.beginClose()

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

	if cancel != nil {
		cancel()
	}

	// The barrier runs before the native teardown rather than after it: closing
	// the process under a turn that is still settling is the very thing waiting
	// for that turn prevents. The wait is real, so a busy session is waited for
	// instead of being answered with backpressure.
	//
	// An abandoned wait ends the close right here. A turn is still holding the
	// slot, so nothing below may run: the teardown would tear the process out from
	// under a commit that is still landing, and the settlement below states
	// quiescence, which is the one fact a session with a live turn cannot state.
	releaseTurn, err := s.awaitSettledTurn(ctx)
	if err != nil {
		return err
	}

	defer releaseTurn()

	closeErr := s.client.Close()
	err = errors.Join(err, closeErr, s.settleSessionClose(ctx, closeErr))

	s.stopNativePump()

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

// settleSessionClose runs the close-fenced settlement in the order the contract
// fixes. The containment boundary has just completed, so: stop the reader that
// served the contained process, commit the resumable snapshot, terminalize what
// the session still owns and state the quiescence fact that completed proof
// produced, and fence the session.
//
// A boundary that did not complete terminalizes nothing, commits nothing new, and
// states no fact — a set of activities the adapter has just proved it cannot
// contain must not be declared terminal — and a snapshot the store does not hold
// means no quiescence fact at all.
func (s *agentSession) settleSessionClose(ctx context.Context, closeErr error) error {
	s.nativePumpHandle().stopReceiving()

	contained := !errors.Is(closeErr, claude.ErrProcessContainmentIncomplete)

	var commitErr error
	if contained {
		commitErr = s.commitSessionMirror()
	}

	return errors.Join(commitErr, s.lifecycleStream().settleClose(ctx, contained, commitErr == nil))
}

func (s *agentSession) recordContainmentError(err error) {
	if s.agent != nil {
		s.agent.recordContainmentError(err)
	}
}
