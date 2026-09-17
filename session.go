package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
)

const (
	// sessionShutdownGrace is how long a claude process gets after SIGTERM before
	// its group is killed.
	sessionShutdownGrace = 2 * time.Second
	// sessionShutdownTimeout bounds one shutdown rung.
	sessionShutdownTimeout = 10 * time.Second
	// sessionAbortTimeout bounds the native abort a cancel or close sends.
	sessionAbortTimeout = 5 * time.Second
	// sessionSettleTimeout bounds one turn's settlement after the native run
	// ended: the stats read, the mirror commit, and the terminal lifecycle
	// event.
	sessionSettleTimeout = 60 * time.Second
	// claudeInitializeTimeout bounds the native control-protocol handshake.
	claudeInitializeTimeout = 60 * time.Second
)

// session is one ACP session: one claude conversation, driven by one live claude
// process at a time.
type session struct {
	callbacks             sync.WaitGroup
	agent                 *Agent
	id                    acp.SessionId
	nativeID              string
	cwd                   string
	additionalDirectories []string
	options               ClaudeOptions
	rawEvents             *wire.RawEvents
	// agentDir is claude's config root for this session's launches.
	agentDir string
	// sessionFile is the native session file claude writes.
	sessionFile string
	// gate admits one foreground operation at a time: a prompt, a config
	// change, or a restore.
	gate chan struct{}

	mu            sync.Mutex
	runtime       *runtime
	mirrored      int
	model         string
	contextWindow int64
	models        []claude.Model
	effort        string
	outputStyle   string
	outputStyles  []string
	commands      []acp.AvailableCommand
	title         string
	updatedAt     string
	closing       bool
	closeDone     chan struct{}
	closeErr      error
	poison        string
	turn          *turn
	cycle         *cycle
	dialogs       map[string]*dialog

	mirrorMu sync.Mutex
	lcMu     sync.Mutex
	lc       lifecycle.Publisher
}

// runtime is one claude process generation.
type runtime struct {
	env             []string
	executable      string
	quotaAccess     *claude.QuotaAccess
	quotaClassified bool
	quotaModel      string
	proc            *process.Process
	client          *claude.Client
	cancel          context.CancelFunc
	// done is closed when the pump has stopped routing this generation.
	done        chan struct{}
	releaseOnce sync.Once
}

// release ends this generation's read loop and closes its pipes. It runs once,
// whichever of the pump and the shutdown ladder reaches it first.
func (rt *runtime) release() {
	rt.releaseOnce.Do(func() {
		rt.cancel()
		_ = rt.proc.Close()
	})
}

// cycle is one foreground run: the work of one accepted prompt, or one
// agent-origin run claude started between prompts.
type cycle struct {
	lifecycle.Cycle
	state cycleState
	// failure records an event-delivery or native control failure.
	failure error
}

type turnEnd int

const (
	turnRunning turnEnd = iota
	turnSettled
	turnTransportEnded
)

// turn is one accepted prompt.
type turn struct {
	cycle
	submission lifecycle.Submission
	accepted   bool
	cancelled  bool
	ended      turnEnd
	settled    chan struct{}
	settleOnce sync.Once
	finished   chan struct{}
	// cancelCtx ends the turn's own context. The session owns it, so a peer
	// request the SDK refuses never cancels a turn in flight.
	cancelCtx context.CancelFunc
}

// cancelTurn ends the turn's context if one was installed.
func (t *turn) cancelTurn() {
	if t.cancelCtx != nil {
		t.cancelCtx()
	}
}

func (t *turn) settle(end turnEnd) {
	t.settleOnce.Do(func() {
		t.ended = end
		close(t.settled)
	})
}

// dialog is one pending native callback, cancellable by session/cancel
// and the shutdown ladder.
type dialog struct {
	cancel context.CancelCauseFunc
}

var errDialogCancelled = errors.New("dialog cancelled by the session")

// launch starts one claude process for this session and binds it as the live
// runtime. sessionPath names the native session file to continue, or is
// empty for a new conversation.
func (s *session) launch(ctx context.Context, sessionPath string) (*runtime, error) {
	executable, err := s.agent.ensureExecutable(ctx)
	if err != nil {
		return nil, err
	}

	env, err := s.launchEnvironment()
	if err != nil {
		return nil, err
	}

	if seedErr := process.WriteSeedFiles(s.agentDir, s.agent.options.SeedFiles); seedErr != nil {
		if refusal := wire.SeedFileRefusal(seedErr); refusal != nil {
			return nil, refusal
		}

		return nil, s.startFailure(ctx, seedErr)
	}

	s.mu.Lock()
	s.sessionFile = claude.SessionPath(s.agentDir, s.cwd, s.nativeID)

	if s.model == "" {
		s.model = s.options.Model
		if s.model == "" {
			s.model = s.agent.options.DefaultModel
		}
	}

	launch := claude.Launch{SessionID: s.nativeID, Model: s.model,
		PermissionMode: s.options.PermissionMode, SystemPrompt: s.options.SystemPrompt, Bare: s.options.Bare,
		OutputSchema: s.options.OutputSchema, SettingSources: s.agent.options.ClaudeSettingSources,
		SettingsFile: s.agent.options.ClaudeSettingsFile, AdditionalDirectories: s.additionalDirectories}
	s.mu.Unlock()

	if sessionPath != "" {
		rows, readErr := claude.ReadRows(sessionPath)
		if readErr != nil {
			return nil, s.startFailure(ctx, readErr)
		}

		launch.Resume = len(rows) > 0
	}

	proc, err := process.Start(ctx, process.Request{
		Executable: executable,
		Args:       launch.Args(),
		Env:        env,
		Dir:        s.cwd,
	})
	if err != nil {
		return nil, s.startFailure(ctx, err)
	}

	// The read loop outlives the request that launched claude: the shutdown ladder
	// ends it.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	client := claude.NewClient(proc.Stdin(), proc.Stdout())

	client.Start(readCtx)

	rt := &runtime{env: env, executable: executable, proc: proc, client: client, cancel: cancelRead, done: make(chan struct{})}

	s.mu.Lock()
	closing := s.closing

	if !closing {
		s.runtime = rt
	}
	s.mu.Unlock()

	// A close that began while this launch was in flight has already sampled
	// the runtime it will stop, so this generation is stopped here instead of
	// bound. No pump ever routes it, so its done is closed before the ladder
	// signals, reaps, and releases the process.
	if closing {
		close(rt.done)
		s.stopRuntime(ctx, rt)

		return nil, wire.UnknownSession()
	}

	go s.pump(context.WithoutCancel(ctx), rt)

	return rt, nil
}

// bindRestored records the native file and the row count a restore hydrated,
// under the lock the mirror and relaunch paths read them with.
func (s *session) bindRestored(path string, mirrored int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessionFile = path
	s.mirrored = mirrored
}

// launchEnvironment applies the session overlay to the construction-time environment.
func (s *session) launchEnvironment() ([]string, error) {
	environment := s.agent.environment(s.options.Env, nil)
	environment.ExtraPathDirs = s.options.ExtraPathDirs

	env, err := environment.Build()
	if err != nil {
		return nil, wire.Unsupported(wire.MetaOptionPath(vendor, metaEnvKey))
	}

	return env, nil
}
func (s *session) permissionMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.options.PermissionMode
}

// startFailure maps a failed native launch or setup onto the closed
// off-prompt internal-failure shape. The reason goes to the log.
func (s *session) startFailure(ctx context.Context, err error) error {
	s.agent.log.ErrorContext(ctx, "claude session start failed",
		slog.String("session_id", string(s.id)),
		slog.String("reason", err.Error()),
	)

	return wire.InternalFailure(vendor, internalClassNativeStart)
}

// configureRuntime discovers the native catalogs and current session settings.
func (s *session) configureRuntime(ctx context.Context, rt *runtime, model string) error {
	initCtx, cancel := context.WithTimeout(ctx, claudeInitializeTimeout)
	defer cancel()

	initialized, err := rt.client.Initialize(initCtx)
	if err != nil {
		return s.startFailure(ctx, err)
	}

	s.mu.Lock()
	current, effort, outputStyle := s.model, s.options.Effort, s.outputStyle
	s.mu.Unlock()

	if model != "" && model != current {
		if setErr := rt.client.SetModel(ctx, model); setErr != nil {
			return s.startFailure(ctx, setErr)
		}
	}

	if effort != "" {
		if setErr := rt.client.ApplySettings(ctx, map[string]any{nativeEffortLevel: effort}); setErr != nil {
			return s.startFailure(ctx, setErr)
		}
	}

	settings, err := rt.client.Settings(initCtx)
	if err != nil {
		return s.startFailure(ctx, err)
	}

	if outputStyle != "" && outputStyle != initialized.OutputStyle {
		if err := rt.client.ApplySettings(ctx, map[string]any{nativeOutputStyle: outputStyle}); err != nil {
			return s.startFailure(ctx, err)
		}

		initialized.OutputStyle = outputStyle
	}

	access, accessErr := rt.client.QuotaAccess(initCtx, rt.env, s.options.Bare)
	if accessErr != nil {
		return s.startFailure(ctx, accessErr)
	}

	s.mu.Lock()
	rt.quotaAccess = access
	rt.quotaModel = settings.Effective.Model

	if model != "" {
		s.model = model
	}

	if s.model == "" {
		s.model = nativeDefault
	}

	s.models = initialized.Models
	s.commands = availableCommands(initialized.Commands)

	// A caller that named no mode adopts whatever native reports; a caller that
	// named one keeps it as the carrier a later relaunch is built from.
	if s.options.PermissionMode == "" {
		s.options.PermissionMode = initialized.PermissionMode
	}

	s.effort = s.options.Effort
	if settings.Effective.EffortLevel != "" {
		s.effort = settings.Effective.EffortLevel
	}

	s.outputStyle = initialized.OutputStyle
	s.outputStyles = initialized.AvailableOutputStyles
	s.mu.Unlock()

	return nil
}

// ensureRuntime returns the live runtime, relaunching claude against the native
// session file when the previous generation is gone.
func (s *session) ensureRuntime(ctx context.Context) (*runtime, error) {
	s.mu.Lock()
	rt := s.runtime
	sessionFile := s.sessionFile
	s.mu.Unlock()

	if rt != nil {
		return rt, nil
	}

	rt, err := s.launch(ctx, sessionFile)
	if err != nil {
		return nil, err
	}

	if configureErr := s.configureRuntime(ctx, rt, ""); configureErr != nil {
		s.stopRuntime(context.WithoutCancel(ctx), rt)

		return nil, configureErr
	}

	if openErr := s.publishOpen(ctx); openErr != nil {
		return nil, openErr
	}

	return rt, nil
}

// pump delivers native records in wire order for one process generation.
func (s *session) pump(ctx context.Context, rt *runtime) {
	defer close(rt.done)
	defer rt.release()

	for event := range rt.client.Events() {
		s.handleEvent(ctx, rt, event)
	}

	s.runtimeEnded(ctx, rt)
}

// handleEvent attributes native work to the prompt or an agent-origin cycle.
func (s *session) handleEvent(ctx context.Context, rt *runtime, event claude.Event) {
	s.mu.Lock()
	current := s.runtime == rt
	s.mu.Unlock()

	if !current {
		return
	}

	if event.SessionID != "" && event.SessionID != s.nativeID {
		s.poisonSession(ctx, "native_session_identity_drift")

		return
	}

	s.observeQuota(rt, event)
	s.emitRawEvent(ctx, event)

	if event.Type == "control_cancel_request" {
		s.mu.Lock()
		dialog := s.dialogs[event.RequestID]
		s.mu.Unlock()

		if dialog != nil {
			dialog.cancel(errDialogCancelled)
		}

		return
	}

	if event.Type == nativeControlRequest {
		s.handleControl(ctx, rt, event)

		return
	}

	s.mu.Lock()
	t := s.turn
	c := s.cycle
	closing := s.closing
	s.mu.Unlock()

	if t == nil && c == nil && bearsWork(event) && !closing {
		s.openAgentCycle(ctx)
		s.mu.Lock()
		c = s.cycle
		s.mu.Unlock()
	}

	switch {
	case t != nil:
		if bearsWork(event) {
			s.acceptTurn(ctx, t)
		}

		settled, err := s.projectEvent(ctx, rt, &t.cycle, event)
		s.recordFailure(&t.cycle, err)

		if settled {
			t.settle(turnSettled)

			s.finishDelivery(ctx, rt, t)
		}
	case c != nil:
		settled, err := s.projectEvent(ctx, rt, c, event)
		s.recordFailure(c, err)

		if settled {
			s.settleAgentCycle(ctx, rt, c)
		}
	}
}

func bearsWork(event claude.Event) bool {
	switch event.Type {
	case messageRoleAssistant, messageRoleUser, nativeStreamEvent, nativeResult:
		return true
	}

	return event.Type == nativeSystem && event.Subtype == "task_notification"
}

// openAgentCycle opens the foreground for work claude began with no prompt in
// flight, including delegated task notifications.
func (s *session) openAgentCycle(ctx context.Context) {
	c := &cycle{Cycle: lifecycle.Cycle{Origin: lifecycle.CauseActivity}}
	c.state.tools = make(map[string]*toolState)

	if err := s.lc.OpenAgentCycle(ctx, &c.Cycle); err != nil {
		s.agent.log.ErrorContext(ctx, "open agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

		return
	}

	s.mu.Lock()
	s.cycle = c
	s.mu.Unlock()
}

// settleAgentCycle runs the agent-origin settlement on the pump: usage, the
// mirror commit, then the terminal idle.
func (s *session) settleAgentCycle(ctx context.Context, rt *runtime, c *cycle) {
	settleCtx, cancel := context.WithTimeout(ctx, sessionSettleTimeout)
	defer cancel()

	s.emitUsage(settleCtx, &c.state, nil)

	if err := s.commitMirror(settleCtx); err != nil {
		// The cycle's state is not durable, so the incarnation is fenced and the
		// generation that produced it ends: the child is stopped and its
		// binding dropped while this cycle still holds the foreground, so the
		// next operation relaunches and opens a new incarnation.
		s.lc.Fence()
		s.signalRuntime(settleCtx, rt)
		s.dropRuntime(rt)
		s.agent.log.ErrorContext(settleCtx, "mirror commit after agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	verdict := s.judgeCycle(c, false)

	if err := s.lc.Idle(settleCtx, c.Cycle, verdict.stopReason, verdict.outcome); err != nil {
		s.agent.log.ErrorContext(settleCtx, "terminal idle for agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	s.mu.Lock()
	if s.cycle == c {
		s.cycle = nil
	}
	s.mu.Unlock()
}

// runtimeEnded records that a process generation stopped producing events. An
// in-flight prompt learns its transport ended; an open agent-origin cycle ends
// failed; the incarnation's lifecycle stream is fenced once its last terminal
// event is out.
func (s *session) runtimeEnded(ctx context.Context, rt *runtime) {
	s.mu.Lock()

	generationOwned := s.runtime == rt
	t := s.turn
	c := s.cycle

	closing := s.closing
	if !closing {
		s.cycle = nil
	}
	s.mu.Unlock()
	s.cancelDialogs()
	s.callbacks.Wait()

	if c != nil && !closing {
		_ = s.lc.Idle(ctx, c.Cycle, "", lifecycle.OutcomeFailed)
	}

	if t != nil {
		t.cancelTurn()
		t.settle(turnTransportEnded)

		// A turn that already settled still owes its terminal event, so the
		// incarnation ends only once that turn has published it.
		select {
		case <-t.finished:
		case <-time.After(sessionSettleTimeout):
		}
	}

	// The incarnation ends with the generation that produced it, whatever the
	// turn did, so a relaunch never publishes on the old stream id.
	if generationOwned && !closing {
		s.lc.Fence()
	}

	// The binding is dropped last: a relaunch must not open its incarnation
	// before this one is fenced.
	s.dropRuntime(rt)
}

// dropRuntime unbinds one process generation. Its incarnation is already
// fenced, so the next operation launches into a new one.
func (s *session) dropRuntime(rt *runtime) {
	s.mu.Lock()
	if s.runtime == rt {
		s.runtime = nil
	}
	s.mu.Unlock()
}

// signalRuntime ends one process generation: the group is signalled and the
// root is reaped. It never joins the pump, so the pump itself may call it.
func (s *session) signalRuntime(ctx context.Context, rt *runtime) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionShutdownTimeout)
	defer cancel()

	if err := rt.proc.Shutdown(shutdownCtx, sessionShutdownGrace); err != nil {
		_ = rt.proc.Kill()
	}
}

// stopRuntime runs the native rungs of the shutdown ladder for one
// generation: signal the process group, wait for the root, join the pump, and
// release the pipes.
func (s *session) stopRuntime(ctx context.Context, rt *runtime) {
	s.signalRuntime(ctx, rt)

	joinCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionShutdownTimeout)
	defer cancel()

	select {
	case <-rt.done:
	case <-joinCtx.Done():
	}

	rt.release()
	s.dropRuntime(rt)
}

// abort interrupts the native run under a bounded context detached from the
// caller's cancellation.
func (s *session) abort(ctx context.Context, rt *runtime) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAbortTimeout)
	defer cancel()

	if err := rt.client.Abort(abortCtx); err != nil {
		s.agent.log.DebugContext(abortCtx, "claude abort failed", slog.String("session_id", string(s.id)))
	}
}

// cancel implements session/cancel: it cancels the in-flight turn, resolves
// its pending dialogs, and interrupts claude. It is a silent no-op with no turn.
func (s *session) cancel(ctx context.Context) {
	s.mu.Lock()
	t := s.turn
	rt := s.runtime

	if t == nil || t.cancelled {
		s.mu.Unlock()

		return
	}

	t.cancelled = true
	s.mu.Unlock()
	t.cancelTurn()

	s.cancelDialogs()

	if rt != nil {
		s.abort(ctx, rt)
	}
}

func (s *session) registerDialog(id string, cancel context.CancelCauseFunc) func() {
	s.mu.Lock()
	if s.closing || s.runtime == nil || (s.turn != nil && s.turn.cancelled) {
		s.mu.Unlock()
		cancel(errDialogCancelled)

		return func() {}
	}

	if s.dialogs == nil {
		s.dialogs = make(map[string]*dialog)
	}

	s.callbacks.Add(1)

	entry := &dialog{cancel: cancel}
	s.dialogs[id] = entry
	s.mu.Unlock()

	return sync.OnceFunc(func() {
		defer s.callbacks.Done()

		s.mu.Lock()
		if s.dialogs[id] == entry {
			delete(s.dialogs, id)
		}
		s.mu.Unlock()
	})
}

func (s *session) cancelDialogs() {
	s.mu.Lock()

	dialogs := make([]*dialog, 0, len(s.dialogs))
	for _, d := range s.dialogs {
		dialogs = append(dialogs, d)
	}
	s.mu.Unlock()

	for _, d := range dialogs {
		d.cancel(errDialogCancelled)
	}
}

// admissionError reports why a session admits no further work: it is closing
// or poisoned.
func (s *session) admissionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.poison != "":
		return wire.SessionPoisoned(vendor, s.poison)
	case s.closing:
		return wire.UnknownSession()
	default:
		return nil
	}
}

// poisonSession fences every operation but close and delete.
func (s *session) poisonSession(ctx context.Context, cause string) {
	s.mu.Lock()

	first := s.poison == ""
	if first {
		s.poison = cause
	}
	s.mu.Unlock()

	if !first {
		return
	}

	s.agent.log.ErrorContext(ctx, "claude session poisoned",
		slog.String("session_id", string(s.id)), slog.String("cause", cause))
	s.clearCommands(ctx)
}

// acquireGate admits one foreground operation. limit names the backpressure
// token a refusal carries.
func (s *session) acquireGate(limit string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return nil, wire.UnknownSession()
	}

	return wire.AcquireSessionGate(s.gate, limit)
}

// holdGate takes the foreground of a session no request can reach yet, where
// the gate is always free.
func (s *session) holdGate() func() {
	s.gate <- struct{}{}

	return sync.OnceFunc(func() { <-s.gate })
}

// close runs the shutdown ladder: mark closed, resolve pending dialogs and the
// in-flight turn, stop the process, commit the owed rows, terminalize what the
// stream still owns, and fence it.
func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done

		return s.closeErr
	}

	s.closing = true
	s.closeDone = make(chan struct{})
	t := s.turn
	rt := s.runtime

	if t != nil {
		t.cancelled = true
	}
	s.mu.Unlock()

	if t != nil {
		t.cancelTurn()
	}

	s.cancelDialogs()
	s.callbacks.Wait()

	if t != nil {
		if rt != nil {
			s.abort(ctx, rt)
		}

		select {
		case <-t.finished:
		case <-time.After(sessionSettleTimeout):
		}
	}

	if rt != nil {
		s.stopRuntime(ctx, rt)
	}

	var errs []error

	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	if err := s.commitMirror(commitCtx); err != nil {
		errs = append(errs, err)
	}

	s.mu.Lock()
	c := s.cycle
	s.cycle = nil
	s.mu.Unlock()

	if len(errs) == 0 && c != nil {
		if err := s.lc.Idle(commitCtx, c.Cycle, lifecycle.StopReasonCancelled, lifecycle.OutcomeCancelled); err != nil {
			errs = append(errs, err)
		}
	}

	s.lc.Fence()

	s.mu.Lock()
	s.closeErr = errors.Join(errs...)
	close(s.closeDone)
	s.mu.Unlock()

	return s.closeErr
}

// finishDelivery lets control replies pass while the completed turn commits.
func (s *session) finishDelivery(ctx context.Context, rt *runtime, t *turn) {
	var pending []claude.Event

	events := rt.client.Events()

	for {
		select {
		case <-t.finished:
			for index := range pending {
				s.handleEvent(ctx, rt, pending[index])
			}

			return
		case event, ok := <-events:
			if !ok {
				events = nil

				continue
			}

			pending = append(pending, event)
		case <-rt.proc.Done():
			for index := range pending {
				s.handleEvent(ctx, rt, pending[index])
			}

			return
		}
	}
}
