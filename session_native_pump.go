package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/savid/acp-go-claude/internal/claude"
)

// nativePumpQueue bounds the ordered durable outbox. The receive loop blocks when
// it is full, which back-pressures the native transport instead of dropping a
// frame the store is meant to hold.
const (
	nativePumpQueue               = 256
	nativeTaskStartedSubtype      = "task_started"
	nativeTaskNotificationSubtype = "task_notification"
	nativeTaskIDAlias             = "taskId"
	nativeToolUseIDAlias          = "toolUseId"
	nativeParentToolUseIDAlias    = "parentToolUseId"
)

var (
	errNativeReceiveExited = errors.New("claude native receive loop exited unexpectedly")
	errNativeOutboxExited  = errors.New("claude mirror outbox exited unexpectedly")
)

// nativePump is the session's native event loop. Claude's process outlives every
// prompt it serves, so the session owns the reader rather than the turn: it drains
// the native stream continuously, from session start until the process ends, and a
// frame that arrives between turns is read, raw-event delivered, invariant
// checked, and mirrored exactly like one that arrives inside a turn.
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
	client      *claude.Client
	incarnation *nativeIncarnation
	generation  uint64
	stop        context.CancelFunc
	done        chan struct{}
	// lost closes when the incarnation's source ends, carrying the cause the
	// prompt reports.
	lost chan struct{}
	err  error
	// sink is the active turn's phase-aware delivery boundary. Frames observed
	// before native dispatch stay autonomous, while frames observed after write
	// admission wait until lifecycle acceptance is visible.
	sink *nativeTurnSink
	// attached closes when the next turn publishes its sink. The reader waiting
	// for the session's foreground waits on it too: a prompt takes the foreground
	// before it publishes, so a reader that only waited for the token would hold a
	// frame the prompt it is waiting behind is itself waiting for.
	attached chan struct{}
	// commitErr latches the first store failure. A turn whose streamed state the
	// store does not hold is a failed turn, so the barrier reports it rather than
	// letting the boundary pass.
	commitErr      error
	stoppingOutbox atomic.Bool
}

type nativeTurnSinkPhase uint8

const (
	nativeTurnSinkPrepared nativeTurnSinkPhase = iota
	nativeTurnSinkDispatching
	nativeTurnSinkAccepted
	nativeTurnSinkClosed
)

type nativeTurnSink struct {
	route       string
	incarnation *nativeIncarnation

	mu       sync.Mutex
	phase    nativeTurnSinkPhase
	changed  chan struct{}
	before   []nativeOwnedFrame
	buffered []nativeOwnedFrame
	messages []nativeOwnedFrame
	pending  *nativePendingFrame
}

type nativePendingFrame struct {
	frame nativeOwnedFrame
	done  chan struct{}
}

type nativeSinkAdmission struct {
	pending <-chan struct{}
}

func newNativeTurnSink(route string, incarnation *nativeIncarnation) *nativeTurnSink {
	return &nativeTurnSink{
		route: route, incarnation: incarnation,
		changed: make(chan struct{}),
	}
}

func (s *nativeTurnSink) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *nativeTurnSink) activeQueueLocked() *[]nativeOwnedFrame {
	if s.phase == nativeTurnSinkPrepared {
		return &s.before
	}

	if s.phase == nativeTurnSinkDispatching {
		return &s.buffered
	}

	return &s.messages
}

func (s *nativeTurnSink) admit(frame nativeOwnedFrame) nativeSinkAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue := s.activeQueueLocked()

	if len(*queue) < nativePumpQueue {
		*queue = append(*queue, frame)

		s.signalLocked()

		return nativeSinkAdmission{}
	}

	s.pending = &nativePendingFrame{frame: frame, done: make(chan struct{})}

	return nativeSinkAdmission{pending: s.pending.done}
}

func (s *nativeTurnSink) promotePendingLocked() {
	queue := s.activeQueueLocked()
	if s.pending == nil || len(*queue) >= nativePumpQueue {
		return
	}

	*queue = append(*queue, s.pending.frame)
	close(s.pending.done)
	s.pending = nil
	s.signalLocked()
}

func (s *nativeTurnSink) takeBeforeDispatch() []nativeOwnedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	frames := append([]nativeOwnedFrame(nil), s.before...)
	s.before = nil
	s.promotePendingLocked()
	s.signalLocked()

	return frames
}

func (s *nativeTurnSink) beginDispatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.before) != 0 {
		return false
	}

	s.phase = nativeTurnSinkDispatching

	return true
}

func (s *nativeTurnSink) accept() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phase = nativeTurnSinkAccepted
	s.messages = append(s.messages, s.buffered...)
	s.buffered = nil
	s.promotePendingLocked()
	s.signalLocked()
}

func (s *nativeTurnSink) close() []nativeOwnedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phase = nativeTurnSinkClosed
	frames := make([]nativeOwnedFrame, 0, len(s.before)+len(s.buffered)+len(s.messages)+1)
	frames = append(frames, s.before...)
	frames = append(frames, s.buffered...)
	frames = append(frames, s.messages...)

	if s.pending != nil {
		frames = append(frames, s.pending.frame)
		close(s.pending.done)
		s.pending = nil
	}

	s.before = nil
	s.buffered = nil
	s.messages = nil
	s.signalLocked()

	return frames
}

func (s *nativeTurnSink) next(
	ctx context.Context,
	incarnation *nativeIncarnation,
) (nativeOwnedFrame, error) {
	for {
		s.mu.Lock()
		if len(s.messages) != 0 {
			frame := s.messages[0]
			s.messages[0] = nativeOwnedFrame{}
			s.messages = s.messages[1:]
			s.promotePendingLocked()
			s.signalLocked()
			s.mu.Unlock()

			return frame, nil
		}

		changed := s.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-incarnation.lost:
			return nativeOwnedFrame{}, incarnation.failure()
		case <-ctx.Done():
			return nativeOwnedFrame{}, ctx.Err()
		}
	}
}

func (s *nativeTurnSink) causalRoute() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.phase != nativeTurnSinkDispatching && s.phase != nativeTurnSinkAccepted {
		return ""
	}

	return s.route
}

// nativeIncarnation is the exact process lifetime one pump reader serves. Its
// client identity and fresh generation are carried together. failed closes
// ordinary autonomous delivery immediately while the detached containment waits
// to serialize against a concurrent replacement.
type nativeIncarnation struct {
	client          *claude.Client
	generation      uint64
	ownership       nativeFrameOwnership
	failed          atomic.Bool
	expectedStop    atomic.Bool
	lost            chan struct{}
	done            chan struct{}
	stop            context.CancelFunc
	lostOnce        sync.Once
	superviseOnce   sync.Once
	mirrorReadyOnce sync.Once
	mirrorReady     chan struct{}
	mu              sync.Mutex
	err             error
}

// nativeTaskBinding fixes one background task to the route that armed it. The
// binding lives as long as the incarnation that minted the task id: the
// harness's task-notification result origin is a kind alone, one task notifies
// any number of times, and every notification resolves through the binding its
// task_started established.
type nativeTaskBinding struct {
	route     string
	toolUseID string
}

// nativeFrameOwnership fixes task, parent-tool, and message identities to their
// exact causal route. It is owned by the incarnation's single receive
// goroutine, so the decision is made before any prompt sink, raw projection,
// mapper state, usage, or cost can observe the frame.
type nativeFrameOwnership struct {
	tasks    map[string]nativeTaskBinding
	tools    map[string]string
	messages map[string]string
}

var errNativeFrameOwnership = errors.New("claude native frame lacks exact causal ownership")

func (o *nativeFrameOwnership) resolve(msg claude.Message, currentRoute string) (string, bool, error) {
	if err := validateNativeIdentitySchema(msg); err != nil {
		return "", true, errNativeFrameOwnership
	}

	parentRoute, parentCausal, err := o.resolveParentRoute(msg)
	if err != nil {
		return "", parentCausal, err
	}

	if system, ok := msg.(*claude.SystemMessage); ok {
		if route, handled, resolveErr := o.resolveTaskSystem(system, currentRoute, parentRoute); handled {
			return route, true, resolveErr
		}
	}

	if mirror, ok := msg.(*claude.TranscriptMirrorMessage); ok {
		route, causal, err := o.resolveMirror(mirror)
		if err != nil {
			return "", causal, err
		}

		if causal {
			if err := o.captureFrameIdentity(msg, route); err != nil {
				return "", true, err
			}

			return route, true, nil
		}
	}

	if parentRoute != "" {
		if err := o.captureFrameIdentity(msg, parentRoute); err != nil {
			return "", true, err
		}

		return parentRoute, true, nil
	}

	if err := o.captureFrameIdentity(msg, currentRoute); err != nil {
		return "", false, err
	}

	return "", false, nil
}

func (o *nativeFrameOwnership) resolveParentRoute(msg claude.Message) (string, bool, error) {
	parentToolUseID := nativeParentToolUseID(msg)
	if parentToolUseID == "" {
		return "", false, nil
	}

	route := o.tools[parentToolUseID]
	if route == "" {
		return "", true, errNativeFrameOwnership
	}

	return route, true, nil
}

func (o *nativeFrameOwnership) resolveTaskSystem(
	system *claude.SystemMessage,
	currentRoute string,
	parentRoute string,
) (string, bool, error) {
	switch system.Subtype {
	case nativeTaskStartedSubtype:
		return o.resolveTaskStarted(system, currentRoute, parentRoute)
	case "task_progress", "task_updated", nativeTaskNotificationSubtype:
		return o.resolveTaskContinuation(system, parentRoute)
	default:
		return "", false, nil
	}
}

func (o *nativeFrameOwnership) resolveTaskStarted(
	system *claude.SystemMessage,
	currentRoute string,
	parentRoute string,
) (string, bool, error) {
	taskID := nativeStringField(system.Raw, "task_id")

	toolUseID := nativeStringField(system.Raw, "tool_use_id")
	if taskID == "" || toolUseID == "" {
		return "", true, errNativeFrameOwnership
	}

	if o.tasks == nil {
		o.tasks = make(map[string]nativeTaskBinding)
	}

	if binding, exists := o.tasks[taskID]; exists {
		if binding.toolUseID != toolUseID || (parentRoute != "" && binding.route != parentRoute) {
			return "", true, errNativeFrameOwnership
		}

		return binding.route, true, o.captureFrameIdentity(system, binding.route)
	}

	route := parentRoute
	if route == "" {
		route = currentRoute
	}

	if route == "" {
		return "", true, errNativeFrameOwnership
	}

	if err := o.bindTool(toolUseID, route); err != nil {
		return "", true, err
	}

	o.tasks[taskID] = nativeTaskBinding{route: route, toolUseID: toolUseID}

	return route, true, o.captureFrameIdentity(system, route)
}

func (o *nativeFrameOwnership) resolveTaskContinuation(
	system *claude.SystemMessage,
	parentRoute string,
) (string, bool, error) {
	taskID := nativeStringField(system.Raw, "task_id")
	toolUseID := nativeStringField(system.Raw, "tool_use_id")
	binding, exists := o.tasks[taskID]

	if taskID == "" || !exists || (toolUseID != "" && toolUseID != binding.toolUseID) ||
		(parentRoute != "" && parentRoute != binding.route) {
		return "", true, errNativeFrameOwnership
	}

	return binding.route, true, o.captureFrameIdentity(system, binding.route)
}

func (o *nativeFrameOwnership) bindTool(toolUseID string, route string) error {
	if toolUseID == "" || route == "" {
		return errNativeFrameOwnership
	}

	if o.tools == nil {
		o.tools = make(map[string]string)
	}

	if existing := o.tools[toolUseID]; existing != "" && existing != route {
		return errNativeFrameOwnership
	}

	o.tools[toolUseID] = route

	return nil
}

func (o *nativeFrameOwnership) captureFrameIdentity(msg claude.Message, route string) error {
	if route == "" || msg == nil {
		return nil
	}

	if assistant, ok := msg.(*claude.AssistantMessage); ok {
		for _, block := range assistant.Content {
			if tool, isTool := block.(claude.ToolUseBlock); isTool {
				if err := o.bindTool(tool.ID, route); err != nil {
					return err
				}
			}
		}
	}

	identity := nativeStringField(msg.RawMessage(), "uuid")
	if identity == "" {
		return nil
	}

	if o.messages == nil {
		o.messages = make(map[string]string)
	}

	if existing := o.messages[identity]; existing != "" && existing != route {
		return errNativeFrameOwnership
	}

	o.messages[identity] = route

	return nil
}

// resolveMirror checks one transcript-mirror batch against the identities this
// incarnation has bound. A transcript file interleaves entries from every route
// the session has run, so the file itself has no owner and one batch may span
// routes: each entry that names causal identity must name a binding this
// incarnation knows, and the batch is attributed causally only when every such
// identity agrees on exactly one route. A batch that spans routes, or names
// none, is session store state on the ingress route.
func (o *nativeFrameOwnership) resolveMirror(mirror *claude.TranscriptMirrorMessage) (string, bool, error) {
	routes := make(map[string]struct{})

	for _, rawEntry := range mirror.Entries {
		var entry map[string]any

		_ = json.Unmarshal(rawEntry, &entry)

		parentToolUseID := nativeStringField(entry, "parent_tool_use_id")
		if parentToolUseID != "" {
			route := o.tools[parentToolUseID]
			if route == "" {
				return "", true, errNativeFrameOwnership
			}

			routes[route] = struct{}{}
		}

		taskID := nativeStringField(entry, "task_id")
		if taskID != "" {
			binding, exists := o.tasks[taskID]
			if !exists {
				return "", true, errNativeFrameOwnership
			}

			routes[binding.route] = struct{}{}
		}

		identity := nativeStringField(entry, "uuid")
		if identity != "" {
			if route := o.messages[identity]; route != "" {
				routes[route] = struct{}{}
			}
		}
	}

	if len(routes) != 1 {
		return "", false, nil
	}

	var route string
	for candidate := range routes {
		route = candidate
	}

	return route, true, nil
}

func nativeParentToolUseID(msg claude.Message) string {
	switch typed := msg.(type) {
	case *claude.AssistantMessage:
		if typed.ParentToolUseID != "" {
			return typed.ParentToolUseID
		}
	case *claude.StreamEventMessage:
		if typed.ParentToolUseID != "" {
			return typed.ParentToolUseID
		}
	case *claude.UserMessage:
		if typed.ParentToolUseID != "" {
			return typed.ParentToolUseID
		}
	}

	if msg == nil {
		return ""
	}

	return nativeStringField(msg.RawMessage(), "parent_tool_use_id")
}

func nativeStringField(values map[string]any, key string) string {
	value, _ := values[key].(string)

	return value
}

// validateNativeIdentitySchema refuses a frame that spells causal identity in a
// vocabulary the ownership grammar does not read. Identity is read only at the
// root the resolver consults — the frame's own top level — so an aliased
// identity key there is a frame whose ownership this session cannot prove.
// Everything below that root, including a result's origin and every
// transcript-mirror journal entry, is the harness's own vocabulary and stays
// opaque to the ownership grammar; a mirror entry must still parse, or the
// identity keys the resolver reads from it cannot be proven absent.
func validateNativeIdentitySchema(msg claude.Message) error {
	if msg == nil {
		return nil
	}

	if nativeIdentityAliasesAtRoot(msg.RawMessage()) {
		return errNativeFrameOwnership
	}

	if mirror, ok := msg.(*claude.TranscriptMirrorMessage); ok {
		for _, rawEntry := range mirror.Entries {
			if !json.Valid(rawEntry) {
				return fmt.Errorf("%w: transcript mirror identity", errNativeFrameOwnership)
			}
		}
	}

	return nil
}

func nativeIdentityAliasesAtRoot(values map[string]any) bool {
	if values == nil {
		return false
	}

	for _, key := range []string{nativeTaskIDAlias, nativeToolUseIDAlias, nativeParentToolUseIDAlias} {
		if _, exists := values[key]; exists {
			return true
		}
	}

	return false
}

type nativeOwnedFrame struct {
	message         claude.Message
	route           string
	causal          bool
	foregroundOwned bool
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
// call it without knowing whether session start or a relaunch got there first,
// and the whole step is serialized: the idempotence check, the retirement of the
// previous identity, the new one, and the reader that serves it are one
// transition, not four a second caller can interleave with.
func (s *agentSession) serveNativePump(ctx context.Context, client *claude.Client) error {
	if client == nil {
		return nil
	}

	s.pumpServeMu.Lock()
	defer s.pumpServeMu.Unlock()

	pump := s.nativePumpHandle()

	pump.mu.Lock()
	served := pump.client == client
	pump.mu.Unlock()

	if served {
		return nil
	}

	if err := s.endNativeIncarnationLocked(ctx); err != nil {
		return errors.Join(err, s.closeNativeClient(client))
	}

	route := s.establishmentRoute(client)
	if route == "" {
		var err error

		route, err = newUUID()
		if err != nil {
			return errors.Join(err, s.closeNativeClient(client))
		}
	}

	receiveCtx, stop := context.WithCancel(context.WithoutCancel(ctx))

	// Close and publication share this transition. If serve wins, the snapshot,
	// exact route, and reader are all visible before close begins. If close wins,
	// none of them is published.
	s.callbackOwnershipMu.Lock()
	defer s.callbackOwnershipMu.Unlock()

	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()

	if closing {
		stop()

		return errors.Join(closedSessionError(), s.closeNativeClient(client))
	}

	// The identity names this process lifetime and the opening snapshot is what
	// tells the host that lifetime exists, so both land before the reader that
	// serves it is published: a process drained under no announced incarnation is
	// native work the host was never told about. A snapshot that does not reach the
	// host therefore contains the process it could not name, and leaves the pump
	// serving nothing.
	if err := s.lifecycleStream().incarnate(ctx); err != nil {
		stop()

		return errors.Join(err, s.closeNativeClient(client))
	}

	pump.mu.Lock()
	pump.generation++
	incarnation := &nativeIncarnation{
		client: client, generation: pump.generation, stop: stop,
		lost: make(chan struct{}), done: make(chan struct{}),
		mirrorReady: make(chan struct{}),
	}
	pump.client = client
	pump.incarnation = incarnation
	pump.stop = stop
	pump.done = incarnation.done
	pump.lost = incarnation.lost
	pump.err = nil
	pump.mu.Unlock()

	s.setAutonomousRoute(route, incarnation)
	client.AdoptControlRoute(route)

	// Tool-use and workflow correlation names identities the retired process
	// minted, and the replacement process reuses none of them.
	release := s.takeForeground()
	s.excursion = nil
	s.resetSessionToolUpdateOptions()
	release()

	// A between-prompt failure latched the incarnation it contained. That
	// incarnation is retired now and this one owes the host nothing the retired
	// one failed to deliver, so the refusal it carried is released with it.
	s.clearAutonomousFailure(client)

	go pump.receive(receiveCtx, incarnation)

	return nil
}

// endNativeIncarnation retires the current incarnation: the reader stops, the
// identity is retired, and anything the incarnation still held terminalizes as
// failed. A session with no incarnation open is unchanged.
func (s *agentSession) endNativeIncarnationLocked(ctx context.Context) error {
	pump := s.nativePumpHandle()

	incarnation := pump.currentIncarnation()
	if incarnation == nil {
		return nil
	}

	_, err := s.retireExactNativeIncarnationLocked(ctx, incarnation)

	return err
}

// endExactNativeIncarnationLocked retires expected only while it is still the
// pump generation being served. The caller holds pumpServeMu, so a successful
// identity check covers the reader stop and lifecycle retirement as one step.
func (s *agentSession) endExactNativeIncarnationLocked(
	ctx context.Context,
	expected *nativeIncarnation,
) (bool, error) {
	return s.retireExactNativeIncarnationLocked(ctx, expected)
}

func (s *agentSession) retireExactNativeIncarnationLocked(
	ctx context.Context,
	expected *nativeIncarnation,
) (bool, error) {
	pump := s.nativePumpHandle()
	if expected == nil || !pump.serves(expected) {
		return false, nil
	}

	s.callbackOwnershipMu.Lock()
	expected.failed.Store(true)
	s.callbackOwnershipMu.Unlock()
	s.cancelPendingInteractionsExact(expected)

	pump.stopReceivingExact(expected)

	closeErr := s.closeNativeClient(expected.client)

	if commitErr := s.commitSessionMirror(); commitErr != nil {
		s.lifecycleStream().abandonIncarnation()
		s.clearAutonomousRoute(expected)
		expected.signalMirrorReady()

		return true, errors.Join(storeCommitError(commitErr), closeErr)
	}

	if errors.Is(closeErr, ErrContainmentIncomplete) {
		s.lifecycleStream().abandonIncarnation()
		s.clearAutonomousRoute(expected)
		expected.signalMirrorReady()

		return true, closeErr
	}

	// A foreground prompt may own the lifecycle turn that must retire. It waits
	// only for the reader/mirror boundary, settles and releases the foreground,
	// then this path retires the lifecycle identity and route in that order.
	expected.signalMirrorReady()

	release := s.takeForeground()
	s.excursion = nil
	retireErr := s.lifecycleStream().loseIncarnation(ctx)

	release()
	s.clearAutonomousRoute(expected)
	expected.signalMirrorReady()

	return true, errors.Join(retireErr, closeErr)
}

func (i *nativeIncarnation) signalMirrorReady() {
	if i == nil {
		return
	}

	i.mirrorReadyOnce.Do(func() { close(i.mirrorReady) })
}

// stopReceiving ends the reader for the current incarnation and waits for it, so
// a boundary taken after it provably covers everything that incarnation
// delivered.
func (p *nativePump) stopReceiving() {
	p.mu.Lock()

	stop, done, incarnation := p.stop, p.done, p.incarnation
	if incarnation != nil {
		incarnation.expectedStop.Store(true)
	}

	p.client = nil
	p.incarnation = nil
	p.stop = nil
	p.done = nil
	p.mu.Unlock()

	if stop == nil {
		return
	}

	stop()
	<-done
}

// stopReceivingExact stops expected only if no replacement has taken the pump.
// It is the containment primitive that prevents delayed failure work for A from
// stopping B's reader.
func (p *nativePump) stopReceivingExact(expected *nativeIncarnation) bool {
	p.mu.Lock()
	if p.incarnation != expected {
		p.mu.Unlock()

		return false
	}

	stop, done := p.stop, p.done

	expected.expectedStop.Store(true)

	p.client = nil
	p.incarnation = nil
	p.stop = nil
	p.done = nil
	p.mu.Unlock()

	if stop != nil {
		stop()
		<-done
	}

	return true
}

// drainReceiving waits for the controller and reader to deliver the complete
// prefix admitted before native close. It cancels the reader only when the
// bounded join itself fails, and reports that abort so close cannot publish
// quiescence over frames it did not project.
func (p *nativePump) drainReceiving(ctx context.Context) error {
	p.mu.Lock()
	incarnation := p.incarnation
	done := p.done
	p.mu.Unlock()

	if incarnation == nil || done == nil {
		return nil
	}

	drainCtx, cancel := settlementContext(ctx)
	defer cancel()

	select {
	case <-done:
	case <-drainCtx.Done():
		incarnation.stop()
		<-done

		return errors.Join(errNativeReceiveExited, context.Cause(drainCtx))
	}

	p.mu.Lock()
	if p.incarnation == incarnation {
		p.client = nil
		p.incarnation = nil
		p.stop = nil
		p.done = nil
	}
	p.mu.Unlock()

	return nil
}

func (p *nativePump) currentIncarnation() *nativeIncarnation {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.incarnation
}

func (p *nativePump) expectStopCurrent() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.incarnation != nil {
		p.incarnation.expectedStop.Store(true)
	}
}

// currentNativeIncarnation reports the exact reader identity without creating a
// pump for stream-only tests or sessions that have not launched native work.
func (s *agentSession) currentNativeIncarnation() *nativeIncarnation {
	s.mu.Lock()
	pump := s.pump
	s.mu.Unlock()

	if pump == nil {
		return nil
	}

	return pump.currentIncarnation()
}

func (p *nativePump) serves(expected *nativeIncarnation) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return expected != nil && p.incarnation == expected
}

// receive drains the native source until it ends. Every frame gets the work that
// belongs to the session — the raw event, the native invariant check, and the
// durable mirror append — before the active turn, if there is one, sees it.
func (p *nativePump) receive(
	ctx context.Context,
	incarnation *nativeIncarnation,
) {
	var receiveErr error

	defer func() {
		recovered := recover()

		panicked := recovered != nil
		if recovered != nil {
			receiveErr = errNativeReceiveExited
		}

		if receiveErr != nil {
			p.recordIncarnationEnd(incarnation, receiveErr)
		}

		if panicked {
			p.session.failNativeIncarnation(ctx, incarnation, receiveErr, "receive")
		} else if receiveErr != nil && !incarnation.expectedStop.Load() {
			p.failIncarnation(ctx, incarnation, receiveErr, "receive")
		}

		close(incarnation.done)
	}()

	for {
		msg, err := incarnation.client.Receive(ctx)
		if err != nil {
			receiveErr = err

			return
		}

		p.dispatch(ctx, incarnation, msg)
	}
}

// dispatch does the session's own work for one frame and hands it to the active
// turn. A transcript mirror frame is store state rather than conversation state,
// so it goes to the outbox and no further.
func (p *nativePump) dispatch(ctx context.Context, incarnation *nativeIncarnation, msg claude.Message) {
	if incarnation == nil || incarnation.failed.Load() {
		return
	}

	frame, err := p.captureOwnedFrame(incarnation, msg)
	if err != nil {
		p.failIncarnation(withTurnRoute(ctx, frame.route), incarnation, err, "ownership")

		return
	}

	p.deliver(ctx, incarnation, frame)
}

// captureOwnedFrame fixes every frame to the route that owns ingress, including
// frames with no native causal identifiers. Delivery may wait behind that
// route's full sink, but a later foreground can never become the frame's owner.
func (p *nativePump) captureOwnedFrame(
	incarnation *nativeIncarnation,
	msg claude.Message,
) (nativeOwnedFrame, error) {
	frame := nativeOwnedFrame{message: msg}

	currentRoute := ""

	p.mu.Lock()
	sink := p.sink

	if sink != nil && sink.incarnation == incarnation {
		currentRoute = sink.causalRoute()
		frame.foregroundOwned = currentRoute != ""
	}
	p.mu.Unlock()

	if currentRoute == "" {
		currentRoute, _ = p.session.autonomousRouteExact(incarnation)
	}

	frame.route = currentRoute

	route, causal, err := incarnation.ownership.resolve(msg, currentRoute)
	if err != nil {
		return frame, err
	}

	if route != "" {
		frame.route = route
	}

	frame.causal = causal

	if frame.route == "" {
		return frame, errNativeFrameOwnership
	}

	return frame, nil
}

// deliver hands one conversation frame to its captured owner. A prompt's turn
// reads it from that turn's sink; with no prompt owning ingress, the session maps
// it itself under an agent-origin turn. Nothing is dropped: the frame stays on
// the reader until its owner has taken it, which back-pressures the native
// transport rather than losing a frame behind a departing turn.
//
// A full sink may keep the reader waiting while its prompt departs. That closes
// the sink door and projects the frame under its already captured route; it is
// never offered to a replacement foreground. A reader that finds no sink waits
// for either the foreground token or the sink publication, but the publication
// changes only how the captured frame is projected, never who owns it.
func (p *nativePump) deliver(ctx context.Context, incarnation *nativeIncarnation, frame nativeOwnedFrame) {
	if frame.route == "" {
		p.failIncarnation(ctx, incarnation, errNativeFrameOwnership, "ownership")

		return
	}

	for {
		if incarnation.failed.Load() {
			return
		}

		p.mu.Lock()
		sink := p.sink
		attached := p.attachedSignalLocked()

		if sink != nil {
			if sink.incarnation != incarnation || sink.route == "" {
				p.mu.Unlock()
				p.failIncarnation(ctx, incarnation, errNativeReceiveExited, "ownership")

				return
			}

			if frame.route != sink.route {
				p.mu.Unlock()
				p.projectCapturedFrame(ctx, incarnation, frame)

				return
			}

			admission := sink.admit(frame)
			p.mu.Unlock()

			if admission.pending == nil {
				return
			}

			select {
			case <-admission.pending:
				return
			case <-ctx.Done():
				return
			}
		}
		p.mu.Unlock()

		token := p.session.foregroundToken()

		select {
		case token <- struct{}{}:
			_, owned := p.session.autonomousRouteExact(incarnation)
			if !owned {
				<-token
				p.failIncarnation(ctx, incarnation, errNativeReceiveExited, "ownership")

				return
			}

			if frame.foregroundOwned || frame.causal {
				p.projectCapturedFrame(ctx, incarnation, frame)
				<-token

				return
			}

			ownerCtx := withTurnRoute(ctx, frame.route)

			conversation, err := p.projectOwnedFrame(ownerCtx, incarnation, frame.message)
			if err == nil && conversation {
				p.session.observeAutonomousFrame(ownerCtx, incarnation, frame.message)
			}

			<-token

			if err != nil {
				p.failIncarnation(ownerCtx, incarnation, err, "projection")
			}

			return
		case <-attached:
		case <-ctx.Done():
			return
		}
	}
}

// projectCapturedFrame reports a frame whose original foreground departed or
// differs from the currently attached foreground. It emits no lifecycle
// transition: raw, mirror, typed, usage, and cost projection all retain the
// captured route, and any failure contains the producing incarnation.
func (p *nativePump) projectCapturedFrame(
	ctx context.Context,
	incarnation *nativeIncarnation,
	frame nativeOwnedFrame,
) {
	ownerCtx := withTurnRoute(ctx, frame.route)

	conversation, err := p.projectOwnedFrame(ownerCtx, incarnation, frame.message)
	if err == nil && conversation {
		err = p.session.mapCausalBackgroundFrame(ownerCtx, frame.message)
	}

	if err != nil {
		p.failIncarnation(ownerCtx, incarnation, err, "projection")
	}
}

func (p *nativePump) projectOwnedFrame(
	ctx context.Context,
	incarnation *nativeIncarnation,
	msg claude.Message,
) (bool, error) {
	if err := p.session.emitRawClaudeMessage(ctx, msg); err != nil {
		return false, err
	}

	if err := p.session.checkNativeSessionInvariant(ctx, msg); err != nil {
		p.recordCommitError(err)

		return false, err
	}

	frame, mirror := msg.(*claude.TranscriptMirrorMessage)
	if !mirror {
		return true, nil
	}

	if err := p.enqueue(ctx, nativePumpWork{frame: frame}); err != nil {
		return false, err
	}

	return false, nil
}

func (p *nativePump) enqueue(ctx context.Context, work nativePumpWork) error {
	if err := p.storeError(); err != nil {
		return err
	}

	select {
	case p.work <- work:
	case <-p.workDone:
		return p.terminalStoreError()
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-p.workDone:
		return p.terminalStoreError()
	default:
		return nil
	}
}

// attachedSignalLocked reports the channel that closes when the next turn
// publishes its frame sink.
func (p *nativePump) attachedSignalLocked() chan struct{} {
	if p.attached == nil {
		p.attached = make(chan struct{})
	}

	return p.attached
}

// outbox is the ordered durable store writer. It appends in arrival order and
// answers each barrier with the durability of everything queued before it, so a
// boundary that observes no error provably covers the whole prefix the barrier
// followed. On shutdown it writes what is already queued rather than discarding
// it: the reader has already stopped, so that queue is finite and it is the last
// state the session owes the store.
func (p *nativePump) outbox(ctx context.Context) {
	expectedExit := false

	defer func() {
		recovered := recover()
		if recovered != nil || !expectedExit {
			p.recordCommitError(errNativeOutboxExited)

			if incarnation := p.currentIncarnation(); incarnation != nil {
				p.failIncarnation(ctx, incarnation, errNativeOutboxExited, "outbox")
			}
		}

		close(p.workDone)
	}()

	for {
		select {
		case work := <-p.work:
			p.apply(ctx, work)
		case <-p.quit:
			p.stoppingOutbox.Store(true)
			p.drain(ctx)

			expectedExit = true

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

		if incarnation := p.currentIncarnation(); incarnation != nil {
			p.failIncarnation(ctx, incarnation, err, "mirror")
		}
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

	if err := p.enqueue(ctx, nativePumpWork{barrier: answer}); err != nil {
		return err
	}

	select {
	case err := <-answer:
		return err
	case <-p.workDone:
		return p.terminalStoreError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// commitSessionMirror is the session's single commit point. Every prompt exit and
// the close boundary run through it, and it is bounded by its own deadline rather
// than by the turn or the request that reached it.
func (s *agentSession) commitSessionMirror() error {
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(context.Background()), sessionSettlementTimeout)
	defer cancel()

	return s.nativePumpHandle().barrier(ctx)
}

// attachTurn installs the active turn's frame sink and returns its idempotent
// retirement. Admission and removal share the pump lock, so retirement owns the
// complete accepted prefix, including the reader's one backpressure slot.
func (p *nativePump) attachTurn(
	route string,
	incarnation *nativeIncarnation,
) (*nativeTurnSink, func()) {
	sink := newNativeTurnSink(route, incarnation)

	p.mu.Lock()
	p.sink = sink
	published := p.attachedSignalLocked()
	p.attached = nil
	p.mu.Unlock()

	// The publication is announced after the sink is installed, so a reader woken
	// by it re-reads a foreground this turn already holds.
	close(published)

	var releaseOnce sync.Once

	return sink, func() {
		releaseOnce.Do(func() {
			p.mu.Lock()

			frames := sink.close()
			if p.sink == sink {
				p.sink = nil
			}
			p.mu.Unlock()

			retirementCtx, cancelRetirement := settlementContext(
				withTurnRoute(context.Background(), sink.route),
			)
			defer cancelRetirement()

			for _, frame := range frames {
				if frame.foregroundOwned || frame.causal {
					p.projectCapturedFrame(retirementCtx, sink.incarnation, frame)

					continue
				}

				ownerCtx := withTurnRoute(retirementCtx, frame.route)

				conversation, err := p.projectOwnedFrame(ownerCtx, sink.incarnation, frame.message)
				if err == nil && conversation {
					p.session.observeAutonomousFrame(ownerCtx, sink.incarnation, frame.message)
				}

				if err != nil {
					p.failIncarnation(ownerCtx, sink.incarnation, err, "projection")
				}
			}
		})
	}
}

// next reports the turn's next native frame. A turn ends on its own context, on
// the loss of the incarnation serving it, or on a frame; it never reads the
// native source itself.
func (p *nativePump) next(
	ctx context.Context,
	sink *nativeTurnSink,
	incarnation *nativeIncarnation,
) (claude.Message, error) {
	if incarnation == nil {
		return nil, claude.ErrMessageStreamClosed
	}

	for {
		frame, err := sink.next(ctx, incarnation)
		if err != nil {
			return nil, err
		}

		conversation, err := p.projectOwnedFrame(ctx, incarnation, frame.message)
		if err != nil {
			p.failIncarnation(ctx, incarnation, err, "projection")

			return nil, err
		}

		if conversation {
			return frame.message, nil
		}
	}
}

func (i *nativeIncarnation) failure() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.err == nil {
		return claude.ErrMessageStreamClosed
	}

	return i.err
}

func (p *nativePump) failIncarnation(
	ctx context.Context,
	incarnation *nativeIncarnation,
	cause error,
	classification string,
) {
	if incarnation == nil || cause == nil || incarnation.expectedStop.Load() {
		return
	}

	p.recordIncarnationEnd(incarnation, cause)
	p.session.failNativeIncarnation(ctx, incarnation, cause, classification)
}

func (p *nativePump) recordIncarnationEnd(incarnation *nativeIncarnation, cause error) {
	if incarnation == nil || cause == nil {
		return
	}

	incarnation.mu.Lock()
	if incarnation.err == nil {
		incarnation.err = cause
	}
	incarnation.mu.Unlock()
	incarnation.lostOnce.Do(func() { close(incarnation.lost) })

	p.mu.Lock()
	if p.incarnation == incarnation {
		p.err = cause
	}
	p.mu.Unlock()
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

func (p *nativePump) terminalStoreError() error {
	if err := p.storeError(); err != nil {
		return err
	}

	if !p.stoppingOutbox.Load() {
		return errNativeOutboxExited
	}

	return nil
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
	pump.quitOnce.Do(func() {
		pump.stoppingOutbox.Store(true)
		close(pump.quit)
	})
	<-pump.workDone
}
