package claudeacp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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
	// stderrTailBytes is how much of claude's stderr the session retains for the
	// process-exit cause.
	stderrTailBytes = 8 << 10
)

// session is one ACP session: one claude conversation, driven by one live claude
// process at a time.
type session struct {
	agent                 *Agent
	id                    acp.SessionId
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
	lc       lifecycleState
}

// runtime is one claude process generation.
type runtime struct {
	proc      *process.Process
	client    *claude.Client
	stderr    *stderrTail
	cancel    context.CancelFunc
	callbacks sync.WaitGroup
	// done is closed when the pump has stopped routing this generation.
	done chan struct{}
}

// cycle is one foreground run: the work of one accepted prompt, or one
// agent-origin run claude started between prompts.
type cycle struct {
	turnID  string
	cycleID string
	origin  lifecycle.Cause
	state   cycleState
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
	timedOut   bool
	ended      turnEnd
	settled    chan struct{}
	settleOnce sync.Once
	finished   chan struct{}
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

// stderrTail retains the last bytes claude wrote to stderr.
type stderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.data = append(t.data, p...)
	if len(t.data) > stderrTailBytes {
		t.data = t.data[len(t.data)-stderrTailBytes:]
	}

	return len(p), nil
}

// lastLine is the final non-empty stderr line, which is where a dying harness
// names its reason.
func (t *stderrTail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	lines := strings.Split(strings.TrimSpace(string(t.data)), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

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
		var refused *process.SeedFileError
		if errors.As(seedErr, &refused) {
			return nil, wire.Unsupported("seedFiles")
		}

		return nil, s.startFailure(ctx, seedErr)
	}

	if s.id == "" {
		s.id = acp.SessionId(uuid.NewString())
	}

	s.sessionFile = claude.SessionPath(s.agentDir, s.cwd, string(s.id))
	if s.model == "" {
		s.model = s.options.Model
		if s.model == "" {
			s.model = s.agent.options.DefaultModel
		}
	}

	proc, err := process.Start(ctx, process.Request{
		Executable: executable,
		Args: claude.Launch{SessionID: string(s.id), Resume: sessionPath != "", Model: s.model,
			PermissionMode: s.options.PermissionMode, SystemPrompt: s.options.SystemPrompt, Bare: s.options.Bare,
			OutputSchema: s.options.OutputSchema, SettingSources: s.agent.options.ClaudeSettingSources,
			SettingsFile: s.agent.options.ClaudeSettingsFile, AdditionalDirectories: s.additionalDirectories}.Args(),
		Env: env,
		Dir: s.cwd,
	})
	if err != nil {
		return nil, s.startFailure(ctx, err)
	}

	tail := &stderrTail{}

	go func() { _, _ = io.Copy(tail, proc.Stderr()) }()

	// The read loop outlives the request that launched claude: the shutdown ladder
	// ends it.
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	client := claude.NewClient(proc.Stdin(), proc.Stdout())

	client.Start(readCtx)

	s.mu.Lock()

	rt := &runtime{proc: proc, client: client, stderr: tail, cancel: cancelRead, done: make(chan struct{})}
	s.runtime = rt
	s.mu.Unlock()

	go s.pump(context.WithoutCancel(ctx), rt)

	return rt, nil
}

// launchEnvironment applies the session overlay to the construction-time environment.
func (s *session) launchEnvironment() ([]string, error) {
	environment := s.agent.environment(s.options.Env, nil)
	environment.ExtraPathDirs = s.options.ExtraPathDirs

	env, err := environment.Build()
	if err != nil {
		return nil, wire.Unsupported(metaOptionPath(metaEnvKey))
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
func (s *session) configureRuntime(ctx context.Context, rt *runtime, model string, expectID string) error {
	initCtx, cancel := context.WithTimeout(ctx, s.agent.options.ClaudeInitializeTimeout)
	defer cancel()

	initialized, err := rt.client.Initialize(initCtx)
	if err != nil {
		return s.startFailure(ctx, err)
	}

	if expectID != "" && string(s.id) != expectID {
		return s.startFailure(ctx, errors.New("native session id mismatch"))
	}

	if model != "" && model != s.model {
		if setErr := rt.client.SetModel(ctx, model); setErr != nil {
			return s.startFailure(ctx, setErr)
		}
	}

	if s.options.Effort != "" {
		if setErr := rt.client.ApplySettings(ctx, map[string]any{nativeEffortLevel: s.options.Effort}); setErr != nil {
			return s.startFailure(ctx, setErr)
		}
	}

	settings, err := rt.client.Settings(initCtx)
	if err != nil {
		return s.startFailure(ctx, err)
	}

	if s.outputStyle != "" && s.outputStyle != initialized.OutputStyle {
		if err := rt.client.ApplySettings(ctx, map[string]any{nativeOutputStyle: s.outputStyle}); err != nil {
			return s.startFailure(ctx, err)
		}

		initialized.OutputStyle = s.outputStyle
	}

	s.mu.Lock()
	if model != "" {
		s.model = model
	}

	if s.model == "" {
		s.model = nativeDefault
	}

	s.models = initialized.Models
	s.commands = availableCommands(initialized.Commands)
	s.options.PermissionMode = initialized.PermissionMode

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
	s.mu.Unlock()

	if rt != nil {
		return rt, nil
	}

	rt, err := s.launch(ctx, s.sessionFile)
	if err != nil {
		return nil, err
	}

	if configureErr := s.configureRuntime(ctx, rt, "", string(s.id)); configureErr != nil {
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

	if event.SessionID != "" && event.SessionID != string(s.id) {
		s.poisonSession(ctx, "native_session_identity_drift")

		return
	}

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
		if err != nil && t.failure == nil {
			t.failure = err
		}

		if settled {
			t.settle(turnSettled)

			s.finishDelivery(ctx, rt, t)
		}
	case c != nil:
		settled, err := s.projectEvent(ctx, rt, c, event)
		if err != nil && c.failure == nil {
			c.failure = err
		}

		if settled {
			s.settleAgentCycle(ctx, c)
		}
	}
}

func bearsWork(event claude.Event) bool {
	switch event.Type {
	case messageRoleAssistant, messageRoleUser, nativeStreamEvent, nativeResult:
		return true
	}

	return event.Type == "system" && event.Subtype == "task_notification"
}

// openAgentCycle opens the foreground for work claude began with no prompt in
// flight, including delegated task notifications.
func (s *session) openAgentCycle(ctx context.Context) {
	c := &cycle{origin: lifecycle.CauseActivity}
	c.state.tools = make(map[string]*toolState)

	if err := s.lcOpenAgentCycle(ctx, c); err != nil {
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
func (s *session) settleAgentCycle(ctx context.Context, c *cycle) {
	settleCtx, cancel := context.WithTimeout(ctx, sessionSettleTimeout)
	defer cancel()

	s.emitUsage(settleCtx, &c.state, nil)

	if err := s.commitMirror(settleCtx); err != nil {
		s.lcFence()
		s.agent.log.ErrorContext(settleCtx, "mirror commit after agent-origin cycle failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	verdict := judgeCycle(c, false)

	if err := s.lcIdle(settleCtx, c, verdict); err != nil {
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
	s.cancelDialogs()
	rt.callbacks.Wait()
	s.mu.Lock()
	if s.runtime == rt {
		s.runtime = nil
	}

	t := s.turn
	c := s.cycle
	s.cycle = nil
	closing := s.closing
	s.mu.Unlock()

	if c != nil && !closing {
		_ = s.lcIdle(ctx, c, cycleVerdict{outcome: lifecycle.OutcomeFailed})
	}

	if t != nil {
		// The prompt settles the turn and fences the stream after its idle.
		t.settle(turnTransportEnded)

		return
	}

	if !closing {
		s.lcFence()
	}
}

// stopRuntime runs the native rungs of the shutdown ladder for one
// generation: signal the process group, wait for the root, join the pump, and
// release the pipes.
func (s *session) stopRuntime(ctx context.Context, rt *runtime) {
	defer rt.cancel()

	shutdownCtx, cancel := context.WithTimeout(ctx, sessionShutdownTimeout)
	defer cancel()

	if err := rt.proc.Shutdown(shutdownCtx, sessionShutdownGrace); err != nil {
		_ = rt.proc.Kill()
	}

	select {
	case <-rt.done:
	case <-shutdownCtx.Done():
		rt.cancel()
	}

	_ = rt.proc.Close()

	s.mu.Lock()
	if s.runtime == rt {
		s.runtime = nil
	}
	s.mu.Unlock()
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

	s.cancelDialogs()

	if rt != nil {
		s.abort(ctx, rt)
	}
}

// timeout ends a turn that exceeded the configured deadline.
func (s *session) timeout(ctx context.Context, t *turn) {
	s.mu.Lock()
	rt := s.runtime

	if s.turn != t || t.cancelled || t.timedOut {
		s.mu.Unlock()

		return
	}

	t.timedOut = true
	s.mu.Unlock()

	s.cancelDialogs()

	if rt != nil {
		s.abort(ctx, rt)
	}
}

func (s *session) registerDialog(id string, cancel context.CancelCauseFunc) func() {
	s.mu.Lock()
	if s.dialogs == nil {
		s.dialogs = make(map[string]*dialog)
	}

	entry := &dialog{cancel: cancel}
	s.dialogs[id] = entry
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		if s.dialogs[id] == entry {
			delete(s.dialogs, id)
		}
		s.mu.Unlock()
	}
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
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	default:
		return nil, wire.Backpressure(limit)
	}
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

	s.cancelDialogs()

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

	if c != nil {
		if err := s.lcIdle(commitCtx, c, cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}); err != nil {
			errs = append(errs, err)
		}
	}

	s.lcFence()

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
			return
		}
	}
}
