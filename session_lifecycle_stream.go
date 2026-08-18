package claudeacp

import (
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/lifecycle"
)

// defaultSessionSettlementTimeout bounds one settlement's emissions. It is the
// commit boundary's budget, because a settlement's emissions state what that
// commit made durable and the two are the same close: a settlement that outran
// the budget its own durable prefix was given is not slow, it is wedged.
const defaultSessionSettlementTimeout = defaultSessionMirrorCommitTimeout

// sessionSettlementTimeout is the live bound. It is a var so a test can prove
// the bound exists without waiting out the real budget.
var sessionSettlementTimeout = defaultSessionSettlementTimeout

// The entities one incarnation mints are named from the incarnation identity, so
// a reader can see at a glance which stream a cycle, a turn, or an action
// belongs to.
const (
	lifecycleCycleSuffix  = "/cycle/"
	lifecycleTurnSuffix   = "/turn/"
	lifecycleActionSuffix = "/action/"
)

// lifecycleOutcome is the truthful end of one foreground cycle: the ACP v1 stop
// reason the response carries and the outcome recorded on the ending turn. Both
// terminators derive it from the same final response pair, so the terminal event
// and the v1 answer can never disagree.
type lifecycleOutcome struct {
	stopReason string
	outcome    lifecycle.Outcome
}

// lifecycleOutcomeFor derives one cycle's recorded end from the turn's final
// response pair. No ACP v1 stop reason names a failure, so a failed cycle records
// its outcome and states no stop reason at all rather than borrowing one.
func lifecycleOutcomeFor(resp acp.PromptResponse, err error) lifecycleOutcome {
	if err != nil {
		return lifecycleOutcome{outcome: lifecycle.OutcomeFailed}
	}

	reason := string(resp.StopReason)

	switch resp.StopReason {
	case acp.StopReasonCancelled:
		return lifecycleOutcome{stopReason: reason, outcome: lifecycle.OutcomeCancelled}
	case acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		return lifecycleOutcome{stopReason: reason, outcome: lifecycle.OutcomeLimit}
	case acp.StopReasonRefusal:
		return lifecycleOutcome{stopReason: reason, outcome: lifecycle.OutcomeRefused}
	default:
		return lifecycleOutcome{stopReason: reason, outcome: lifecycle.OutcomeSuccess}
	}
}

// sessionStream is one session's lifecycle stream. The session owns it, not a
// prompt: Claude's native process outlives every turn it serves, so the stream
// identity follows that process and rotates only when the process it names ends.
//
// Every method holds mu for its whole body, delivery included, so foreground
// state, action ownership, the claimed sequence, the reducer's projection, and
// the notification that carries them all move in one linearized order. Nothing
// can reorder a state transition around a delivery, and nothing can observe a
// turn between the native dispatch that opened it and the acceptance that
// announced it.
//
// A nil sessionStream is a connection where the host offered nothing: every
// method is inert on it, so the prompt and action paths carry no conditional of
// their own.
type sessionStream struct {
	session    *agentSession
	negotiated lifecycle.Negotiated

	mu sync.Mutex
	// stream follows the session across incarnations and validates every event
	// through the reducer the canonical vectors drive.
	stream *lifecycle.Stream
	// live is true while an incarnation is open. A lost incarnation emits
	// nothing until the next one opens; a fenced session never opens one again.
	live   bool
	fenced bool
	// refused latches an event this adapter could not state truthfully. Fail
	// closed means fail closed: the refusal belongs to the session, and every
	// later emission on it — the next incarnation's opening snapshot included —
	// reports the same refusal.
	refused error
	// lost latches the first emission of the current incarnation that never
	// reached the host. An incarnation with a hole in it is not a stream, so every
	// later emission on it fails rather than continuing a sequence the host never
	// received. The latch ends with the incarnation it belongs to: the next one
	// opens on a fresh identity and re-asserts the whole state in its own
	// snapshot, so it owes the host nothing the lost one failed to deliver.
	lost error
	// cycleID is the foreground cycle the stream currently reports.
	cycleID string
	// minted counts the entities this incarnation has named.
	minted  int
	turn    *streamTurn
	actions map[string]*streamAction
}

// streamTurn is one open foreground cycle. The nonce is the route the prompt
// authenticated with, which is what makes an inbound control callback's owner a
// fact rather than an assumption about which prompt happens to be running.
type streamTurn struct {
	turnID   string
	cycleID  string
	nonce    string
	runID    string
	blockers int
}

// streamAction is one announced action awaiting an answer. It holds the record
// the stream published so a resolution restates the immutable identity with its
// first-sight values.
type streamAction struct {
	update lifecycle.ActionUpdate
	turn   *streamTurn
}

// newSessionStream builds the stream for a negotiated connection.
func newSessionStream(session *agentSession, negotiated lifecycle.Negotiated) *sessionStream {
	return &sessionStream{
		session:    session,
		negotiated: negotiated,
		stream:     lifecycle.NewStream(negotiated),
		actions:    make(map[string]*streamAction),
	}
}

// lifecycleStream reports the session's stream, or nil on a connection whose
// answer omitted the key. A connection with no answer carries no envelope, no
// correlation read, and no lifecycle fact at all.
func (s *agentSession) lifecycleStream() *sessionStream {
	if s.agent == nil {
		return nil
	}

	negotiated := s.agent.negotiatedLifecycle()
	if !negotiated.Present() {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lifecycle == nil {
		s.lifecycle = newSessionStream(s, negotiated)
	}

	return s.lifecycle
}

// incarnate opens the incarnation that names one native process lifetime and
// emits its snapshot, which is that incarnation's first lifecycle-bearing
// notification. A session whose close containment completed opens no further
// incarnation of itself.
func (p *sessionStream) incarnate(ctx context.Context) error {
	if p == nil {
		return nil
	}

	id, err := newUUID()
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The delivery failure that ended the previous incarnation does not follow
	// this one. It latched a stream that no longer exists, and this snapshot is
	// the whole state on a fresh identity, so there is no hole left for it to
	// describe. A refusal is a different thing and does follow: it is about what
	// this adapter may state at all, not about what one incarnation delivered.
	p.lost = nil

	if err := p.emittable(); err != nil {
		return err
	}

	p.stream.Incarnate(id)
	p.live = true
	p.minted = 0
	p.turn = nil
	p.actions = make(map[string]*streamAction)
	p.cycleID = p.mint(lifecycleCycleSuffix)

	// A fresh incarnation opens on the boundary that was actually proven, and
	// nothing is proven about a process that has not run yet: the opening fact is
	// negative until a completed containment boundary states otherwise.
	return p.emitLocked(ctx, lifecycle.SnapshotEvent(p.cycleID, lifecycle.QuiescenceFact{}))
}

// dispatch is the acceptance linearization point. The native dispatch and the
// acceptance that announces its turn run in one critical section, so a control
// callback racing the frame it caused can never announce an action against a turn
// this stream has not opened. A native dispatcher that refused the frame creates
// neither submission nor turn.
//
// The stream is asked whether it can still speak before the frame is written. A
// stream that already lost an event, and a session whose close containment
// completed, can announce no acceptance at all, and a frame written under either
// would be native work no lifecycle event can ever describe. The caller contains
// the frame it did manage to write when a later emission fails; nothing this
// adapter can foresee is left to that path.
func (p *sessionStream) dispatch(
	ctx context.Context,
	submission lifecycle.Submission,
	nonce string,
	send func() error,
) (string, error) {
	if p == nil {
		return "", send()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.emittable(); err != nil {
		return "", err
	}

	if err := send(); err != nil {
		return "", err
	}

	turn := &streamTurn{
		turnID:  p.mint(lifecycleTurnSuffix),
		cycleID: p.cycleID,
		nonce:   nonce,
		runID:   submission.RunID,
	}

	if err := p.emitLocked(ctx, lifecycle.AcceptedEvent(submission, turn.turnID)); err != nil {
		return "", err
	}

	p.turn = turn

	return turn.turnID, p.emitLocked(ctx, lifecycle.TransitionEvent(lifecycle.ForegroundRunning, turn.cycleID, turn.turnID))
}

// settleTurn ends the open turn. Every action still blocking the cycle
// terminalizes first, because the resolution is the reason the foreground may
// move at all, and then the ending idle records the outcome the v1 response
// carries.
func (p *sessionStream) settleTurn(ctx context.Context, turnID string, outcome lifecycleOutcome) error {
	if p == nil || turnID == "" {
		return nil
	}

	ctx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.turn == nil || p.turn.turnID != turnID {
		return nil
	}

	if err := p.terminalizeLocked(ctx, p.turn, actionEndFor(outcome.outcome)); err != nil {
		return err
	}

	turn := p.turn
	p.turn = nil

	return p.emitLocked(ctx, lifecycle.IdleEvent(turn.cycleID, turn.turnID, outcome.stopReason, outcome.outcome))
}

// announceAction registers one inbound native control request against a fresh
// action identity and emits the announcing action_update, so a host never sees an
// action id the adapter cannot yet resolve. The owner and the blocking fact are
// read from the callback's own turn route: this adapter admits a control callback
// only for the turn that authenticated it, and that turn's native dispatcher is
// waiting on the answer, so the action blocks exactly that cycle.
//
// A callback the stream cannot attribute to an open turn announces nothing and
// carries no correlation value: an owner is a fact, never a guess about which
// prompt happens to be running.
func (p *sessionStream) announceAction(
	ctx context.Context,
	nonce string,
	kind lifecycle.ActionKind,
) (lifecycle.ActionUpdate, error) {
	if p == nil || nonce == "" {
		return lifecycle.ActionUpdate{}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	turn := p.turn
	if turn == nil || turn.nonce != nonce {
		return lifecycle.ActionUpdate{}, nil
	}

	if err := p.emittable(); err != nil {
		return lifecycle.ActionUpdate{}, err
	}

	blocks := true
	update := lifecycle.ActionUpdate{
		ActionID:         p.mint(lifecycleActionSuffix),
		Kind:             kind,
		State:            lifecycle.ActionPending,
		Owner:            lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: turn.turnID},
		RunID:            turn.runID,
		BlocksForeground: &blocks,
	}

	p.actions[update.ActionID] = &streamAction{update: update, turn: turn}
	turn.blockers++

	if err := p.emitLocked(ctx, lifecycle.ActionEvent(update)); err != nil {
		return lifecycle.ActionUpdate{}, err
	}

	if turn.blockers > 1 {
		// The cycle already reports requires_action: a second blocker adds to the
		// set that holds it there rather than transitioning again.
		return update, nil
	}

	return update, p.emitLocked(ctx,
		lifecycle.TransitionEvent(lifecycle.ForegroundRequiresAction, turn.cycleID, turn.turnID))
}

// resolveAction terminalizes one announced action exactly once and releases the
// cycle it blocked. A resolution arriving after the settlement or the close that
// already terminalized it changes nothing, because terminal is immutable.
func (p *sessionStream) resolveAction(
	ctx context.Context,
	update lifecycle.ActionUpdate,
	state lifecycle.ActionState,
) error {
	if p == nil || update.ActionID == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.resolveLocked(ctx, update.ActionID, state, true)
}

// resolveLocked terminalizes one pending action. It emits the accompanying
// running transition only when the resolution actually released the cycle and
// that cycle is still open: a turn that ends on this resolution reports its
// terminal idle from the settlement instead.
func (p *sessionStream) resolveLocked(
	ctx context.Context,
	actionID string,
	state lifecycle.ActionState,
	release bool,
) error {
	action := p.actions[actionID]
	if action == nil {
		return nil
	}

	delete(p.actions, actionID)

	action.turn.blockers--

	update := action.update
	update.State = state

	if err := p.emitLocked(ctx, lifecycle.ActionEvent(update)); err != nil {
		return err
	}

	if !release || action.turn.blockers > 0 || p.turn != action.turn {
		return nil
	}

	return p.emitLocked(ctx,
		lifecycle.TransitionEvent(lifecycle.ForegroundRunning, action.turn.cycleID, action.turn.turnID))
}

// terminalizeLocked settles every action still blocking one turn, in
// announcement order. A cancelled cycle terminalizes its blockers before it
// reports its terminal idle, and a fenced stream never leaves an action
// addressable.
func (p *sessionStream) terminalizeLocked(
	ctx context.Context,
	turn *streamTurn,
	state lifecycle.ActionState,
) error {
	for _, actionID := range p.pendingLocked(turn) {
		if err := p.resolveLocked(ctx, actionID, state, false); err != nil {
			return err
		}
	}

	return nil
}

// pendingLocked lists one turn's outstanding actions in announcement order, or
// every outstanding action when turn is nil.
func (p *sessionStream) pendingLocked(turn *streamTurn) []string {
	pending := make([]string, 0, len(p.actions))
	for actionID, action := range p.actions {
		if turn == nil || action.turn == turn {
			pending = append(pending, actionID)
		}
	}

	slices.Sort(pending)

	return pending
}

// loseIncarnation ends the incarnation a native process no longer backs. Pending
// actions and an unsettled turn terminalize as failed, which is what tells a host
// a lost end from a contained one, and the identity is retired: the next process
// opens a new incarnation with its own snapshot.
func (p *sessionStream) loseIncarnation(ctx context.Context) error {
	if p == nil {
		return nil
	}

	ctx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.endIncarnationLocked(ctx, lifecycle.ActionFailed, lifecycle.OutcomeFailed)
}

// abandonIncarnation retires the identity without emitting anything. Durability
// outranks the terminal event: where the foreground-prefix commit or the
// containment boundary itself failed, the incarnation ends unsettled and the next
// incarnation's snapshot asserts the truthful state.
func (p *sessionStream) abandonIncarnation() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.live = false
	p.turn = nil
	p.actions = make(map[string]*streamAction)
}

// endIncarnationLocked terminalizes what the incarnation still holds and retires
// its identity.
func (p *sessionStream) endIncarnationLocked(
	ctx context.Context,
	state lifecycle.ActionState,
	outcome lifecycle.Outcome,
) error {
	if !p.live {
		return nil
	}

	defer func() { p.live = false }()

	if err := p.terminalizeLocked(ctx, nil, state); err != nil {
		return err
	}

	turn := p.turn
	p.turn = nil

	if turn == nil {
		return nil
	}

	return p.emitLocked(ctx, lifecycle.IdleEvent(turn.cycleID, turn.turnID, stopReasonForOutcome(outcome), outcome))
}

// settleClose runs the close-fenced settlement in the one order the contract
// fixes. The containment boundary has already completed when this runs, so it
// terminalizes what the session still owns as cancelled, states the quiescence
// fact the completed proof produced, and fences the session.
//
// A boundary that did not complete terminalizes nothing and states no fact: a set
// of activities the adapter has just proved it cannot contain must not be
// declared terminal, because their next real event would be a post-terminal
// mutation. A resumable snapshot the store does not hold means no quiescence fact
// at all, never a fact with a missing snapshot behind it.
//
// A settlement fact is a fact about a live incarnation, so an incarnation that
// was lost, abandoned, fenced, or never opened receives none. Continuing a
// retired identity would state a fact a conforming reducer must refuse as stale,
// and naming no identity at all would state a malformed one. Close does not need
// the emission to succeed: the durable containment evidence carries the boundary,
// and the next incarnation's snapshot asserts the truthful state.
func (p *sessionStream) settleClose(ctx context.Context, contained bool, committed bool) error {
	if p == nil {
		return nil
	}

	ctx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()

	p.mu.Lock()
	defer p.mu.Unlock()

	defer func() {
		p.fenced = true
		p.live = false
		p.stream.Fence()
	}()

	if !contained || !p.live {
		return nil
	}

	if err := p.endIncarnationLocked(ctx, lifecycle.ActionCancelled, lifecycle.OutcomeCancelled); err != nil {
		return err
	}

	if !committed || !p.negotiated.AuthoritativeQuiescence {
		return nil
	}

	return p.emitLocked(ctx, lifecycle.QuiescenceEvent(lifecycle.QuiescenceFact{
		Quiescent: true,
		Source:    p.negotiated.QuiescenceSource,
		Watermark: p.stream.State().ReducedThrough,
	}))
}

// correlation renders the action correlation value stamped on the outbound
// request that announced update, beside whatever reserved envelopes that surface
// already carries.
func (p *sessionStream) correlation(update lifecycle.ActionUpdate) map[string]any {
	if p == nil || update.ActionID == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelationValue(p.stream.ID(), update)}
}

// mint names the next entity of this incarnation.
func (p *sessionStream) mint(kind string) string {
	p.minted++

	return p.stream.ID() + kind + strconv.Itoa(p.minted)
}

// emittable reports why this stream may not emit. A refused event is refused for
// the life of the session, an incarnation that already lost an event never
// continues its sequence, and a closed session is over.
func (p *sessionStream) emittable() error {
	switch {
	case p.refused != nil:
		return p.refused
	case p.lost != nil:
		return p.lost
	case p.fenced:
		return lifecycleViolationError("the session's close containment completed")
	default:
		return nil
	}
}

// emitLocked claims the next sequence, validates the event through the reducer
// the canonical vectors drive, and delivers it on its own identity-only carrier.
// An event this adapter cannot state truthfully, and one the host never received,
// both fail the caller here and latch: emission failure is loss, and loss fails
// closed rather than leaving a hole a consumer cannot see. They latch separately
// because they outlive different things — a refusal outlives the session, a lost
// delivery only the incarnation whose sequence it holed.
func (p *sessionStream) emitLocked(ctx context.Context, event lifecycle.Event) error {
	if err := p.emittable(); err != nil {
		return err
	}

	envelope, err := p.stream.Emit(event)
	if err != nil {
		p.refused = lifecycleViolationError(err.Error())

		return p.refused
	}

	conn := p.session.agent.connection()
	if conn == nil {
		p.lost = lifecycleViolationError("the ACP connection is unavailable")

		return p.lost
	}

	// The envelope rides the notification's own `_meta`, beside sessionId and
	// update, and the carrier sets neither title nor updatedAt: a carrier mutates
	// no state, so it can never be coalesced away with the envelope on it.
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.session.id,
		Meta:      map[string]any{lifecycle.MetaKey: envelope},
		Update:    acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	}); err != nil {
		p.lost = lifecycleViolationError(err.Error())

		return p.lost
	}

	return nil
}

// actionEndFor reports the terminal action state a settling cycle gives the
// blockers it still holds. Cancel terminalizes as cancelled; every other end of a
// turn that still holds an unanswered request leaves it failed.
func actionEndFor(outcome lifecycle.Outcome) lifecycle.ActionState {
	if outcome == lifecycle.OutcomeCancelled {
		return lifecycle.ActionCancelled
	}

	return lifecycle.ActionFailed
}

// stopReasonForOutcome reports the ACP v1 stop reason an ending idle carries with
// outcome. A failed outcome states none: no v1 reason names a failure and the v1
// error carries it instead.
func stopReasonForOutcome(outcome lifecycle.Outcome) string {
	if outcome == lifecycle.OutcomeCancelled {
		return lifecycle.StopReasonCancelled
	}

	return ""
}

// settlementContext detaches a settlement's emissions from the request that
// asked for it, and bounds them. Settlement reports what has already happened —
// a terminal idle, a retired incarnation, a proven quiescence — and a host that
// cancels its request un-happens none of it. The request's values are kept, so
// the emission still carries its trace; only the cancellation is dropped,
// because a cancelled delivery would hole the stream over work that completed
// anyway.
//
// Dropping the cancellation drops the request's deadline with it, so the bound
// is restated here. Detached is not unbounded: a host whose write never returns
// would otherwise hold the stream lock, the turn slot behind it, and the close
// waiting on that slot forever. The budget expiring is an emission that did not
// reach the host, which is loss, and loss already fails closed down the same
// path every other undelivered event takes.
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), sessionSettlementTimeout)
}

func lifecycleViolationError(message string) error {
	return acp.NewInternalError(map[string]any{
		jsonFieldError:   "claude_lifecycle_violation",
		jsonFieldMessage: message,
	})
}

// beginLifecycleAction announces one outbound permission or elicitation on the
// ordered stream and returns the correlation value to stamp on the request beside
// whatever reserved envelopes that surface already carries, plus the resolution
// that terminalizes the action exactly once.
//
// The announcement precedes the request, so the host never sees an action id the
// adapter cannot yet resolve, and the owner is the turn whose route admitted this
// callback rather than whichever prompt happens to be running.
func (s *agentSession) beginLifecycleAction(
	ctx context.Context,
	kind lifecycle.ActionKind,
) (map[string]any, func(context.Context, lifecycle.ActionState) error, error) {
	stream := s.lifecycleStream()

	update, err := stream.announceAction(ctx, turnNonceFromContext(ctx), kind)
	if err != nil {
		return nil, nil, err
	}

	resolve := func(resolveCtx context.Context, state lifecycle.ActionState) error {
		return stream.resolveAction(resolveCtx, update, state)
	}

	return stream.correlation(update), resolve, nil
}

// withLifecycleMeta merges the action correlation into a request's own `_meta`,
// leaving the vendor annotations and the reserved route object beside it.
func withLifecycleMeta(meta map[string]any, lifecycleMeta map[string]any) map[string]any {
	if len(lifecycleMeta) == 0 {
		return meta
	}

	merged := cloneAnyMap(meta)
	if merged == nil {
		merged = map[string]any{}
	}

	for key, value := range lifecycleMeta {
		merged[key] = value
	}

	return merged
}

// permissionActionState reports the terminal state one permission answer records.
// The outcome is read from the structural option union only: `_meta` on the
// response is read by nobody, and a host cannot change an outcome by annotating
// it.
func permissionActionState(
	resp acp.RequestPermissionResponse,
	err error,
	allows func(acp.PermissionOptionId) bool,
) lifecycle.ActionState {
	switch {
	case err != nil && permissionRequestCancelled(err):
		return lifecycle.ActionCancelled
	case err != nil:
		return lifecycle.ActionFailed
	case resp.Outcome.Selected == nil:
		return lifecycle.ActionCancelled
	case allows(resp.Outcome.Selected.OptionId):
		return lifecycle.ActionAccepted
	default:
		return lifecycle.ActionDeclined
	}
}

// permissionAllowsTool reports whether a selected option is one of the two that
// allow the tool call.
func permissionAllowsTool(option acp.PermissionOptionId) bool {
	return option == permissionAllowOnce || option == permissionAllowAlways
}

// elicitationActionState reports the terminal state one elicitation answer
// records, read from the structural action union alone.
func elicitationActionState(resp acp.UnstableCreateElicitationResponse, err error) lifecycle.ActionState {
	switch {
	case err != nil && permissionRequestCancelled(err):
		return lifecycle.ActionCancelled
	case err != nil:
		return lifecycle.ActionFailed
	case resp.Accept != nil:
		return lifecycle.ActionAccepted
	case resp.Decline != nil:
		return lifecycle.ActionDeclined
	default:
		return lifecycle.ActionCancelled
	}
}
