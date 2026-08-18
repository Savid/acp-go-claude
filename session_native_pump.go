package claudeacp

import (
	"context"
	"sync"

	"github.com/savid/acp-go-claude/internal/claude"
)

// nativePumpQueue bounds the ordered durable outbox. The receive loop blocks when
// it is full, which back-pressures the native transport instead of dropping a
// frame the store is meant to hold.
const nativePumpQueue = 256

// nativePump is the session's native event loop. Claude's process outlives every
// prompt it serves, so the session owns the reader rather than the turn: it drains
// the native stream continuously, from session start until the process ends, and a
// frame that arrives between turns is read, raw-event delivered, invariant
// checked, and mirrored exactly like one that arrives inside a turn.
//
// It replaces the two prompt-tail drains. A timed drain could only ever be a
// guess about how long the tail is, and it silently lost whatever arrived after
// the timer; the durable boundary here is a barrier through the same ordered
// outbox the frames travel, so a commit provably covers every frame received
// before it.
//
// Two goroutines run per session at most: one receive loop for the current native
// incarnation, and one outbox that owns every store write in arrival order. The
// outbox runs under a context detached from every turn, so a cancelled prompt or a
// cancelled JSON-RPC request can never abort a commit in flight.
type nativePump struct {
	session *agentSession

	work     chan nativePumpWork
	quit     chan struct{}
	quitOnce sync.Once
	workDone chan struct{}

	mu sync.Mutex
	// client is the native incarnation the receive loop currently serves.
	client *claude.Client
	stop   context.CancelFunc
	done   chan struct{}
	// lost closes when the incarnation's source ends, carrying the cause the
	// prompt reports.
	lost chan struct{}
	err  error
	// sink is the active turn's frame channel, and sinkDone closes when that turn
	// stops consuming. A frame that misses a departing turn has already been
	// persisted and observed; only the turn's own mapping is skipped.
	sink     chan claude.Message
	sinkDone chan struct{}
	// commitErr latches the first store failure. A turn whose streamed state the
	// store does not hold is a failed turn, so the barrier reports it rather than
	// letting the boundary pass.
	commitErr error
}

// nativePumpWork is one item of the ordered outbox: a frame to persist, or a
// barrier that reports the durability of everything queued before it.
type nativePumpWork struct {
	frame   *claude.TranscriptMirrorMessage
	barrier chan error
}

// nativePumpHandle reports the session's pump, starting its outbox on first use.
func (s *agentSession) nativePumpHandle() *nativePump {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pump == nil {
		s.pump = newNativePump(s)
	}

	return s.pump
}

func newNativePump(session *agentSession) *nativePump {
	pump := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork, nativePumpQueue),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}

	// The outbox is deliberately rooted outside every request and every turn: a
	// store write is a durability obligation of the session, and each attempt
	// carries its own deadline, so no caller's cancellation can abort one in
	// flight or leave a retry half done.
	go pump.outbox(context.Background())

	return pump
}

// serve points the pump at the current native incarnation, ending the previous
// one first. It is idempotent for a client already being served, so a prompt can
// call it without knowing whether session start or a relaunch got there first.
func (s *agentSession) serveNativePump(ctx context.Context, client *claude.Client) error {
	if client == nil {
		return nil
	}

	pump := s.nativePumpHandle()

	pump.mu.Lock()
	if pump.client == client {
		pump.mu.Unlock()

		return nil
	}
	pump.mu.Unlock()

	if err := s.endNativeIncarnation(ctx); err != nil {
		return err
	}

	stream := s.lifecycleStream()

	receiveCtx, stop := context.WithCancel(context.WithoutCancel(ctx))

	pump.mu.Lock()
	pump.client = client
	pump.stop = stop
	pump.done = make(chan struct{})
	pump.lost = make(chan struct{})
	pump.err = nil
	done, lost := pump.done, pump.lost
	pump.mu.Unlock()

	go pump.receive(receiveCtx, client, done, lost)

	// The identity names this process lifetime, so it is minted with the reader
	// that serves it and retired with the process it names.
	return stream.incarnate(ctx)
}

// endNativeIncarnation retires the current incarnation: the reader stops, the
// identity is retired, and anything the incarnation still held terminalizes as
// failed. A session with no incarnation open is unchanged.
func (s *agentSession) endNativeIncarnation(ctx context.Context) error {
	s.nativePumpHandle().stopReceiving()

	return s.lifecycleStream().loseIncarnation(ctx)
}

// stopReceiving ends the reader for the current incarnation and waits for it, so
// a boundary taken after it provably covers everything that incarnation
// delivered.
func (p *nativePump) stopReceiving() {
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.client = nil
	p.stop = nil
	p.done = nil
	p.mu.Unlock()

	if stop == nil {
		return
	}

	stop()
	<-done
}

// receive drains the native source until it ends. Every frame gets the work that
// belongs to the session — the raw event, the native invariant check, and the
// durable mirror append — before the active turn, if there is one, sees it.
func (p *nativePump) receive(ctx context.Context, client *claude.Client, done, lost chan struct{}) {
	defer close(done)
	defer recoverAgentGoroutine(ctx, p.session.agent.log, "session native pump")

	for {
		msg, err := client.Receive(ctx)
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			close(lost)

			return
		}

		p.dispatch(ctx, msg)
	}
}

// dispatch does the session's own work for one frame and hands it to the active
// turn. A transcript mirror frame is store state rather than conversation state,
// so it goes to the outbox and no further.
func (p *nativePump) dispatch(ctx context.Context, msg claude.Message) {
	p.session.emitRawClaudeMessage(ctx, msg)

	if err := p.session.checkNativeSessionInvariant(ctx, msg); err != nil {
		p.recordCommitError(err)

		return
	}

	if frame, ok := msg.(*claude.TranscriptMirrorMessage); ok {
		p.work <- nativePumpWork{frame: frame}

		return
	}

	p.mu.Lock()
	sink, sinkDone := p.sink, p.sinkDone
	p.mu.Unlock()

	if sink == nil {
		return
	}

	select {
	case sink <- msg:
	case <-sinkDone:
	case <-ctx.Done():
	}
}

// outbox is the ordered durable store writer. It appends in arrival order and
// answers each barrier with the durability of everything queued before it, so a
// boundary that observes no error provably covers the whole prefix the barrier
// followed. On shutdown it writes what is already queued rather than discarding
// it: the reader has already stopped, so that queue is finite and it is the last
// state the session owes the store.
func (p *nativePump) outbox(ctx context.Context) {
	defer close(p.workDone)
	defer recoverAgentGoroutine(ctx, p.session.agent.log, "session mirror outbox")

	for {
		select {
		case work := <-p.work:
			p.apply(ctx, work)
		case <-p.quit:
			p.drain(ctx)

			return
		}
	}
}

func (p *nativePump) drain(ctx context.Context) {
	for {
		select {
		case work := <-p.work:
			p.apply(ctx, work)
		default:
			return
		}
	}
}

func (p *nativePump) apply(ctx context.Context, work nativePumpWork) {
	if work.barrier != nil {
		work.barrier <- p.storeError()

		return
	}

	if err := p.session.appendSessionMirror(ctx, work.frame); err != nil {
		p.recordCommitError(err)

		// A turn that keeps streaming into a store which is not holding it only
		// widens the gap, so the turn in flight is woken here. The failure is
		// still reported by the settlement's own commit boundary rather than
		// from inside the loop: there is one commit point per exit.
		p.session.cancelActiveTurn()
	}
}

// cancelActiveTurn wakes the turn in flight without marking it cancelled, so the
// settlement reads its outcome from the boundary that failed rather than from a
// user cancel that never happened.
func (s *agentSession) cancelActiveTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// barrier commits everything received before it and reports whether the store
// holds it. The request travels the same ordered queue the frames do, which is
// what makes the answer a fact about a prefix rather than about a moment.
func (p *nativePump) barrier(ctx context.Context) error {
	answer := make(chan error, 1)

	select {
	case p.work <- nativePumpWork{barrier: answer}:
	case <-p.workDone:
		return p.storeError()
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-answer:
		return err
	case <-p.workDone:
		return p.storeError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// commitSessionMirror is the session's single commit point. Every prompt exit and
// the close boundary run through it, and it is bounded by its own deadline rather
// than by the turn or the request that reached it.
func (s *agentSession) commitSessionMirror() error {
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), sessionMirrorCommitTimeout)
	defer cancel()

	return s.nativePumpHandle().barrier(ctx)
}

// attachTurn installs the active turn's frame sink and returns the release that
// uninstalls it. The release closes the turn's own door first, so a frame the
// receive loop is holding is never delivered to a turn that has stopped reading.
func (p *nativePump) attachTurn() (chan claude.Message, func()) {
	sink := make(chan claude.Message)
	sinkDone := make(chan struct{})

	p.mu.Lock()
	p.sink = sink
	p.sinkDone = sinkDone
	p.mu.Unlock()

	return sink, func() {
		close(sinkDone)

		p.mu.Lock()
		if p.sink == sink {
			p.sink = nil
			p.sinkDone = nil
		}
		p.mu.Unlock()
	}
}

// next reports the turn's next native frame. A turn ends on its own context, on
// the loss of the incarnation serving it, or on a frame; it never reads the
// native source itself.
func (p *nativePump) next(ctx context.Context, sink chan claude.Message) (claude.Message, error) {
	p.mu.Lock()
	lost := p.lost
	p.mu.Unlock()

	select {
	case msg := <-sink:
		return msg, nil
	case <-lost:
		return nil, p.incarnationError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// incarnationError reports why the incarnation ended.
func (p *nativePump) incarnationError() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.err == nil {
		return claude.ErrMessageStreamClosed
	}

	return p.err
}

// incarnationEnded reports whether the native source this turn ran on is gone. A
// prompt that ends with no process behind it ends the incarnation with it.
func (p *nativePump) incarnationEnded() bool {
	p.mu.Lock()
	lost := p.lost
	p.mu.Unlock()

	if lost == nil {
		return true
	}

	select {
	case <-lost:
		return true
	default:
		return false
	}
}

func (p *nativePump) recordCommitError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.commitErr == nil {
		p.commitErr = err
	}
}

func (p *nativePump) storeError() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.commitErr
}

// stopNativePump ends the reader and the outbox. It runs after the session's
// containment boundary has completed and its final commit has landed, so nothing
// is still owed to the store when the queue closes.
func (s *agentSession) stopNativePump() {
	s.mu.Lock()
	pump := s.pump
	s.mu.Unlock()

	if pump == nil {
		return
	}

	pump.stopReceiving()
	pump.quitOnce.Do(func() { close(pump.quit) })
	<-pump.workDone
}
