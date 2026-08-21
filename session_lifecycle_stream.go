package claudeacp

import (
	"context"
	"errors"
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

var errExactInteractionContainment = errors.New("the owning native incarnation failed")

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

// streamTurn is one open foreground cycle. The nonce is the route the callback
// surface authenticated with — the prompt's for a submission-origin turn and the
// incarnation's autonomous route for an agent-origin one — which is what makes an
// inbound control callback's owner a fact rather than an assumption about which
// turn happens to be running. The origin is fixed when the turn opens and is the
// cause every one of its transitions carries.
type streamTurn struct {
	turnID      string
	cycleID     string
	nonce       string
	runID       string
	origin      lifecycle.Cause
	incarnation *nativeIncarnation
	blockers    int
}

// streamAction is one announced action awaiting an answer. It holds the record
// the stream published so a resolution restates the immutable identity with its
// first-sight values.
type streamAction struct {
	update lifecycle.ActionUpdate
	turn   *streamTurn
	wire   actionWireIdentity
}

type controlCallbackOwner struct {
	incarnation *nativeIncarnation
	autonomous  bool
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

	// A callback carrying the autonomous route can open an agent-origin turn
	// after Prompt's early foreground check but before this acceptance point.
	// This check shares p.mu with action preparation, so exactly one wins. The prompt
	// that loses is refused before send and leaves the excursion, its blocker and
	// the stream's refusal latches untouched.
	if p.turn != nil && p.turn.origin == lifecycle.CauseActivity {
		return "", backpressureError("session_foreground")
	}

	if err := p.emittable(); err != nil {
		return "", err
	}

	if err := send(); err != nil {
		return "", err
	}

	turn := &streamTurn{
		turnID:      p.mint(lifecycleTurnSuffix),
		cycleID:     p.cycleID,
		nonce:       nonce,
		runID:       submission.RunID,
		origin:      lifecycle.CauseSubmission,
		incarnation: p.session.currentNativeIncarnation(),
	}

	if err := p.emitLocked(ctx, lifecycle.AcceptedEvent(submission, turn.turnID)); err != nil {
		return "", err
	}

	p.turn = turn

	return turn.turnID, p.emitLocked(ctx,
		lifecycle.TransitionEvent(turn.origin, lifecycle.ForegroundRunning, turn.cycleID, turn.turnID))
}

// openAgentTurn opens the agent-origin turn that represents one between-prompt
// excursion, and reports its identity. The turn carries no submission because no
// client input caused it: the `activity`-caused running transition naming a turn
// the stream has not introduced is what opens it, and is the only event other
// than acceptance that opens a turn at all.
//
// A foreground a turn already holds opens nothing. The prompt path settles the
// excursion it pre-empts before it dispatches, so a second turn here would be a
// foreground two turns claim rather than an excursion the host was owed.
func (p *sessionStream) openAgentTurn(ctx context.Context, route string) (string, error) {
	if p == nil {
		return "", nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.openAgentTurnLocked(ctx, route)
}

func (p *sessionStream) openAgentTurnLocked(ctx context.Context, route string) (string, error) {
	if !p.live || p.turn != nil {
		return "", nil
	}

	if err := p.emittable(); err != nil {
		return "", err
	}

	turn := &streamTurn{
		turnID:      p.mint(lifecycleTurnSuffix),
		cycleID:     p.cycleID,
		nonce:       route,
		origin:      lifecycle.CauseActivity,
		incarnation: p.session.autonomousOwner(route),
	}
	p.turn = turn

	return turn.turnID, p.emitLocked(ctx,
		lifecycle.TransitionEvent(turn.origin, lifecycle.ForegroundRunning, turn.cycleID, turn.turnID))
}

// agentTurnID reports the open agent-origin turn, or the empty string when the
// foreground is idle or a prompt holds it. It is how the native pump adopts a
// turn a control callback opened ahead of the first frame of the same excursion:
// one excursion is one turn, whichever of the two first proved it was running.
func (p *sessionStream) agentTurnID() string {
	if p == nil {
		return ""
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.turn == nil || p.turn.origin != lifecycle.CauseActivity {
		return ""
	}

	return p.turn.turnID
}

// callbackOwner resolves one captured route while the session's callback
// ownership primitive excludes prompt dispatch. A running turn is the only
// owner while it exists; otherwise the current autonomous route names the exact
// incarnation that may open an agent-origin turn.
func (p *sessionStream) callbackOwner(nonce string) (controlCallbackOwner, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.live || p.fenced {
		return controlCallbackOwner{}, false
	}

	if p.turn != nil {
		if p.turn.nonce != nonce {
			return controlCallbackOwner{}, false
		}

		return controlCallbackOwner{
			incarnation: p.turn.incarnation,
			autonomous:  p.turn.origin == lifecycle.CauseActivity,
		}, true
	}

	incarnation := p.session.autonomousOwner(nonce)
	if incarnation == nil || incarnation.failed.Load() {
		return controlCallbackOwner{}, false
	}

	return controlCallbackOwner{incarnation: incarnation, autonomous: true}, true
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

	return p.emitLocked(ctx,
		lifecycle.IdleEvent(turn.origin, turn.cycleID, turn.turnID, outcome.stopReason, outcome.outcome))
}

// prepareAction reserves the identity and owner correlation an outbound host
// request carries. It emits no pending action yet: the real JSON-RPC request must
// be registered and written successfully before announcePreparedAction can state
// that the host has a pending action. The owner and the blocking fact are
// read from the callback's own turn route: this adapter admits a control callback
// only for the turn that authenticated it, and that turn's native dispatcher is
// waiting on the answer, so the action blocks exactly that cycle.
//
// A callback the stream cannot attribute to an open turn announces nothing and
// carries no correlation value: an owner is a fact, never a guess about which
// prompt happens to be running.
//
// Ownership is resolved before emittability, and the order is load-bearing. A
// callback that owns nothing on this stream is the same non-event whatever state
// the stream is in, so a stream latched by work this callback has nothing to do
// with must not turn it into a wire-visible error: a retired route stays unowned
// rather than becoming a failure the caller reports. Only a callback that really
// does name a live owner is entitled to an answer about whether this stream can
// still speak.
func (p *sessionStream) prepareAction(
	ctx context.Context,
	nonce string,
	kind lifecycle.ActionKind,
) (lifecycle.ActionUpdate, error) {
	if p == nil || nonce == "" {
		return lifecycle.ActionUpdate{}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The route the callback authenticated with is the whole answer about its
	// owner. A callback naming the turn that holds the foreground belongs to that
	// turn; one naming this incarnation's autonomous route while the foreground is
	// idle is native work running with no prompt behind it, which is an excursion
	// and opens the agent-origin turn that owns it.
	//
	// Every other callback belongs to nothing and announces nothing. A dead
	// prompt's route, a route a newer prompt replaced, and a route from a retired
	// incarnation each name no live owner, and an owner is a fact rather than a
	// guess about which turn happens to be running.
	if p.turn == nil {
		if nonce != p.session.autonomousRoute() {
			return lifecycle.ActionUpdate{}, nil
		}

		if err := p.emittable(); err != nil {
			return lifecycle.ActionUpdate{}, err
		}

		if _, err := p.openAgentTurnLocked(ctx, nonce); err != nil {
			return lifecycle.ActionUpdate{}, err
		}
	}

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

	return update, nil
}

// announcePreparedAction binds a prepared action to the exact JSON-RPC request
// line observed at the transport boundary, then emits pending and blocks its
// owner. A lost owner, duplicate write, wrong request family, or empty request
// identity fails closed before any action is published.
func (p *sessionStream) announcePreparedAction(
	ctx context.Context,
	update lifecycle.ActionUpdate,
	wire actionWireIdentity,
) error {
	if p == nil || update.ActionID == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if wire.requestID == "" || !actionMethodMatches(update.Kind, wire.method) {
		return lifecycleViolationError("lifecycle action has no exact host request write")
	}

	if _, exists := p.actions[update.ActionID]; exists {
		return lifecycleViolationError("lifecycle action host request was announced more than once")
	}

	turn := p.turn
	if turn == nil || update.Owner.Type != lifecycle.OwnerTurn || update.Owner.ID != turn.turnID {
		return lifecycleViolationError("the lifecycle action no longer has its prepared owner")
	}

	if err := p.emittable(); err != nil {
		return err
	}

	p.actions[update.ActionID] = &streamAction{update: update, turn: turn, wire: wire}
	turn.blockers++

	if err := p.emitLocked(ctx, lifecycle.ActionEvent(update)); err != nil {
		return err
	}

	if turn.blockers > 1 {
		return nil
	}

	return p.emitLocked(ctx,
		lifecycle.TransitionEvent(turn.origin, lifecycle.ForegroundRequiresAction, turn.cycleID, turn.turnID))
}

func actionMethodMatches(kind lifecycle.ActionKind, method string) bool {
	switch kind {
	case lifecycle.ActionPermission:
		return method == acp.ClientMethodSessionRequestPermission
	case lifecycle.ActionElicitation:
		return method == acp.ClientMethodElicitationCreate
	default:
		return false
	}
}

func (p *sessionStream) emitCallbackContent(
	ctx context.Context,
	nonce string,
	emit func() error,
) (*nativeIncarnation, error) {
	if p == nil {
		return nil, emit()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.turn == nil {
		if nonce != p.session.autonomousRoute() {
			return nil, lifecycleViolationError("callback route no longer has a live owner")
		}

		if _, err := p.openAgentTurnLocked(ctx, nonce); err != nil {
			return p.session.autonomousOwner(nonce), err
		}
	}

	if p.turn == nil || p.turn.nonce != nonce {
		return nil, lifecycleViolationError("callback route no longer has a live owner")
	}

	incarnation := p.turn.incarnation
	if err := p.emittable(); err != nil {
		return incarnation, err
	}

	return incarnation, emit()
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

	return p.emitLocked(ctx, lifecycle.TransitionEvent(
		action.turn.origin, lifecycle.ForegroundRunning, action.turn.cycleID, action.turn.turnID))
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

	return p.emitLocked(ctx,
		lifecycle.IdleEvent(turn.origin, turn.cycleID, turn.turnID, stopReasonForOutcome(outcome), outcome))
}

// settleClose runs the close-fenced settlement in the one order the contract
// fixes. The containment boundary has already completed when this runs, so it
// terminalizes what the session still owns as cancelled — the terminal idle a
// still-open turn receives included — commits the resumable snapshot behind those
// transitions, states the quiescence fact the completed proof produced, and
// fences the session.
//
// The commit runs here rather than ahead of the caller's settlement because its
// position in that order is the boundary's own rung. The transitions report how
// work the proof already contained ended, so they precede the durable write and
// stand whatever it does; the quiescence fact certifies the boundary itself, so it
// follows that write and a store that refused it leaves the fact unstated and
// fails the close. Every rung runs under one lock, so nothing announced against
// this session can land between a terminal transition and the fence behind it.
//
// The durable rung is owed whatever the emissions do. A settlement fact is a fact
// about a live incarnation, so an incarnation that was lost, abandoned, fenced, or
// never opened receives none — continuing a retired identity would state a fact a
// conforming reducer must refuse as stale, and naming no identity at all would
// state a malformed one — and the commit still lands underneath, because the
// prefix the store is owed is not a message to a host.
func (p *sessionStream) settleClose(ctx context.Context, commit func() error) error {
	if p == nil {
		return commit()
	}

	ctx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()

	p.mu.Lock()
	defer p.mu.Unlock()

	defer p.fenceLocked()

	live := p.live
	terminalErr := settlementEmission(p.endIncarnationLocked(ctx, lifecycle.ActionCancelled, lifecycle.OutcomeCancelled))
	commitErr := commit()

	switch {
	case terminalErr != nil || commitErr != nil:
		return errors.Join(terminalErr, commitErr)
	case !live || !p.negotiated.AuthoritativeQuiescence:
		return nil
	}

	return settlementEmission(p.emitLocked(ctx, lifecycle.QuiescenceEvent(lifecycle.QuiescenceFact{
		Quiescent: true,
		Source:    p.negotiated.QuiescenceSource,
		Watermark: p.stream.State().ReducedThrough,
	})))
}

// fenceClose is the whole settlement a close whose containment boundary did not
// complete leaves behind. It terminalizes nothing and states no fact: a set of
// activities the adapter has just proved it cannot contain must not be declared
// terminal, because their next real event would be a post-terminal mutation. The
// stream is fenced all the same — the session is over either way, and the caller
// reports the containment failure itself.
func (p *sessionStream) fenceClose() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.fenceLocked()
}

// fenceLocked ends the session's stream. A closed handle never reopens: later
// conversation reuse resumes stored state into a new incarnation of a new logical
// session, never into this one.
func (p *sessionStream) fenceLocked() {
	p.fenced = true
	p.live = false
	p.stream.Fence()
}

// settlementEmission reports what a settlement's emission failure says about this
// adapter. A settlement the peer was no longer there to receive is not a failed
// close: the stream still fences and the durable commit still lands, which is
// what actually carries the boundary; the only thing missing is a host to tell,
// and a hang-up on the far end is not a fault of this adapter's to report. A
// delivery that failed while the peer was still reading stays a failure, because
// that one really did leave a host holding a projection with a gap in it.
func settlementEmission(err error) error {
	if errors.Is(err, errLifecyclePeerGone) {
		return nil
	}

	return err
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
		p.refused = lifecycleViolationError("the lifecycle event was refused")

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
		p.lost = emissionLoss(conn, err)

		return p.lost
	}

	return nil
}

// errLifecyclePeerGone marks an emission that could not be delivered because the
// peer had already hung up. It is still loss — the incarnation latches and emits
// nothing further on a stream the host stopped reading — but it is not this
// adapter's failure, so a boundary whose only fault was that nobody was left to
// tell reports it under its own name rather than as a lifecycle violation.
var errLifecyclePeerGone = errors.New("the ACP peer connection has ended")

// emissionLoss names an emission the host never received. A delivery that failed
// while the peer was still there is this adapter holing its own stream and fails
// closed as a violation. A delivery that failed after the peer's reader loop
// ended is a different fact: the connection is over, every later emission on it
// is equally undeliverable, and there is no host left holding a projection with a
// gap in it.
func emissionLoss(conn agentClient, err error) error {
	select {
	case <-conn.Done():
		return errLifecyclePeerGone
	default:
		return lifecycleViolationError("lifecycle delivery failed")
	}
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

// sessionLifecycleAction is one lifecycle action after its ownership has been
// prepared but before its lifecycle announcement is visible. The exact pending
// tool update is published first; the correlated ACP request must then be
// registered and written in full before afterWireWrite announces the action.
type sessionLifecycleAction struct {
	session     *agentSession
	stream      *sessionStream
	update      lifecycle.ActionUpdate
	route       string
	incarnation *nativeIncarnation
	admission   *controlCallbackAdmission
	announced   bool
}

// lifecycleInteractionOwner is the cancellation identity copied into a pending
// host request. The incarnation is the containment target, while route and
// actionID retain the callback cause that registered it.
type lifecycleInteractionOwner struct {
	incarnation *nativeIncarnation
	route       string
	actionID    string
}

func (a sessionLifecycleAction) meta() map[string]any {
	if a.stream == nil || a.update.ActionID == "" {
		return nil
	}

	return a.stream.correlation(a.update)
}

func (a *sessionLifecycleAction) wireAdmission(ctx context.Context, emit func() error) actionWireAdmission {
	if a == nil || a.stream == nil || a.update.ActionID == "" {
		return actionWireAdmission{}
	}

	return actionWireAdmission{
		actionID: a.update.ActionID,
		publish:  emit,
		written: func(_ context.Context, wire actionWireIdentity) error {
			return a.afterWireWrite(ctx, wire)
		},
	}
}

func (a *sessionLifecycleAction) prepareWireAdmission(
	ctx context.Context,
	emit func() error,
) (actionWireAdmission, error) {
	admission := a.wireAdmission(ctx, emit)
	if admission.present() || emit == nil {
		return admission, nil
	}

	return admission, emit()
}

func (a *sessionLifecycleAction) afterWireWrite(
	ctx context.Context,
	wire actionWireIdentity,
) error {
	if a == nil || a.admission == nil || a.admission.session != a.session {
		return lifecycleViolationError("lifecycle action has no exact callback admission")
	}

	a.session.callbackOwnershipMu.Lock()

	if !a.exactOwnerCurrentLocked() {
		a.session.callbackOwnershipMu.Unlock()

		return lifecycleViolationError("lifecycle action owner is no longer current")
	}

	if err := a.stream.announcePreparedAction(ctx, a.update, wire); err != nil {
		a.session.callbackOwnershipMu.Unlock()
		a.failOwner(ctx, err, "action_announcement")

		return err
	}

	a.announced = true
	a.session.callbackOwnershipMu.Unlock()

	return nil
}

func (a *sessionLifecycleAction) failOwner(ctx context.Context, err error, classification string) {
	if a == nil || a.incarnation == nil || err == nil {
		return
	}

	a.session.failNativeIncarnation(ctx, a.incarnation, err, classification)
}

// exactOwnerCurrentLocked revalidates the registered callback reservation and
// the owner it captured. The caller holds callbackOwnershipMu, the same
// primitive prompt dispatch and close use to rotate or revoke ownership.
func (a *sessionLifecycleAction) exactOwnerCurrentLocked() bool {
	if a == nil || a.admission == nil || a.admission.session != a.session {
		return false
	}

	if _, live := a.session.callbackAdmissions[a.admission]; !live {
		return false
	}

	if a.incarnation != nil &&
		(a.incarnation.failed.Load() || !a.session.nativePumpHandle().serves(a.incarnation)) {
		return false
	}

	return true
}

// responseOwnerCurrent revalidates the exact native incarnation after the host
// wait and before any answer can mutate adapter state or be accepted by Claude.
// The callback admission may still be registered while its old process is being
// retired, so the incarnation identity is the decisive boundary.
func (a *sessionLifecycleAction) responseOwnerCurrent() bool {
	if a == nil || a.session == nil {
		return false
	}

	a.session.callbackOwnershipMu.Lock()
	defer a.session.callbackOwnershipMu.Unlock()

	return a.exactOwnerCurrentLocked()
}

func (a *sessionLifecycleAction) resolve(ctx context.Context, state lifecycle.ActionState) error {
	if a == nil || a.stream == nil || a.update.ActionID == "" {
		return nil
	}

	resolutionCtx, cancel := settlementContext(ctx)
	defer cancel()

	a.session.callbackOwnershipMu.Lock()
	a.session.mu.Lock()
	closing := a.session.closing
	a.session.mu.Unlock()

	if closing || (a.incarnation != nil && a.incarnation.failed.Load()) {
		a.session.callbackOwnershipMu.Unlock()

		return nil
	}

	if !a.exactOwnerCurrentLocked() {
		a.session.callbackOwnershipMu.Unlock()

		failure := lifecycleViolationError("lifecycle action resolution lost its exact owner")
		a.failOwner(ctx, failure, "action_resolution")

		return failure
	}

	if !a.announced {
		a.session.callbackOwnershipMu.Unlock()

		failure := lifecycleViolationError("lifecycle action host request was not announced")
		a.failOwner(ctx, failure, "action_registration")

		return failure
	}

	err := a.stream.resolveAction(resolutionCtx, a.update, state)
	a.session.callbackOwnershipMu.Unlock()

	if err != nil && a.incarnation != nil {
		a.session.failNativeIncarnation(ctx, a.incarnation, err, "action_resolution")
	}

	return err
}

func (a *sessionLifecycleAction) interactionOwner() lifecycleInteractionOwner {
	return lifecycleInteractionOwner{
		incarnation: a.incarnation,
		route:       a.route,
		actionID:    a.update.ActionID,
	}
}

// beginLifecycleAction prepares one outbound permission or elicitation under the
// exact controller admission. Its lifecycle pending event is emitted later,
// only after the correlated host JSON-RPC request is observed written in full.
//
// The request write precedes the announcement, so pending never claims a host
// request that failed to reach the wire. The owner is the turn whose registered
// callback admission prepared the action, never whichever prompt is current
// when the host later answers.
func (s *agentSession) beginLifecycleAction(
	ctx context.Context,
	kind lifecycle.ActionKind,
) (*sessionLifecycleAction, error) {
	stream := s.lifecycleStream()

	admission := controlCallbackAdmissionFromContext(ctx)
	if admission == nil || admission.session != s || admission.route == "" {
		return nil, lifecycleViolationError("callback has no exact registered admission")
	}

	var update lifecycle.ActionUpdate

	err := func() error {
		var announceErr error

		s.callbackOwnershipMu.Lock()
		defer s.callbackOwnershipMu.Unlock()

		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()

		if closing {
			return closedSessionError()
		}

		if _, live := s.callbackAdmissions[admission]; !live {
			return lifecycleViolationError("callback admission is no longer live")
		}

		incarnation := admission.incarnation
		if incarnation != nil && incarnation.failed.Load() {
			return lifecycleViolationError("callback native incarnation is no longer live")
		}

		if incarnation != nil && !s.nativePumpHandle().serves(incarnation) {
			return lifecycleViolationError("callback native incarnation is no longer current")
		}

		update, announceErr = stream.prepareAction(ctx, admission.route, kind)

		return announceErr
	}()
	if err != nil {
		if admission.incarnation != nil {
			s.failNativeIncarnation(ctx, admission.incarnation, err, "action_preparation")
		}

		return nil, err
	}

	return &sessionLifecycleAction{
		session:     s,
		stream:      stream,
		update:      update,
		route:       admission.route,
		incarnation: admission.incarnation,
		admission:   admission,
	}, nil
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

// interactionActionState distinguishes exact-incarnation containment from a
// user/session cancellation. Both wake the host request through context
// cancellation, but only the latter truthfully terminalizes the action as
// cancelled; losing the process that owned it is a failed action.
func interactionActionState(ctx context.Context, state lifecycle.ActionState) lifecycle.ActionState {
	if errors.Is(context.Cause(ctx), errExactInteractionContainment) {
		return lifecycle.ActionFailed
	}

	return state
}
