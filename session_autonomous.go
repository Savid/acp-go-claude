package claudeacp

import (
	"context"
	"log/slog"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

// agentExcursion is one between-prompt run of native work: the agent-origin turn
// that represents it and the frame state that turn accumulates. It exists only
// while native work is running with no prompt behind it, and it is replaced
// rather than reused, so one excursion is always exactly one turn.
type agentExcursion struct {
	turnID                 string
	lastAssistantMessageID string
	lastAssistantModel     string
	stream                 usageSnapshot
	streamKnown            bool
	streamEmittedTotal     int
}

// autonomousRoute reports the callback route this incarnation's native work uses
// while no prompt owns the foreground. It is minted with the incarnation and
// retired with it, so a callback carrying a route from a retired incarnation
// names no live owner.
func (s *agentSession) autonomousRoute() string {
	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	return s.autonomousNonce
}

func (s *agentSession) setAutonomousRoute(nonce string, incarnation *nativeIncarnation) {
	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	s.autonomousNonce = nonce
	s.autonomousIncarnation = incarnation
}

// rotateAutonomousRoute advances the between-prompt callback epoch for expected.
// A callback captured before a prompt dispatch therefore stays stale after that
// prompt hands control back instead of becoming live again on a reused route.
func (s *agentSession) rotateAutonomousRoute(expected *nativeIncarnation, nonce string) bool {
	if expected == nil || nonce == "" {
		return false
	}

	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	if s.autonomousIncarnation != expected {
		return false
	}

	s.autonomousNonce = nonce

	return true
}

// autonomousOwner reports the exact pump incarnation that minted nonce. The
// route and identity move together under this leaf lock, so a callback never
// resolves its failure target by consulting a later session client.
func (s *agentSession) autonomousOwner(nonce string) *nativeIncarnation {
	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	if nonce == "" || nonce != s.autonomousNonce {
		return nil
	}

	return s.autonomousIncarnation
}

func (s *agentSession) autonomousRouteExact(expected *nativeIncarnation) (string, bool) {
	if expected == nil {
		return "", false
	}

	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	if s.autonomousIncarnation != expected || s.autonomousNonce == "" {
		return "", false
	}

	return s.autonomousNonce, true
}

// clearAutonomousRoute retires only expected's route. A delayed retirement for
// A therefore cannot clear the route B published after it.
func (s *agentSession) clearAutonomousRoute(expected *nativeIncarnation) {
	if expected == nil {
		return
	}

	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	if s.autonomousIncarnation != expected {
		return
	}

	s.autonomousNonce = ""
	s.autonomousIncarnation = nil
}

// foregroundToken reports the session's one foreground slot.
func (s *agentSession) foregroundToken() chan struct{} {
	s.autonomousMu.Lock()
	defer s.autonomousMu.Unlock()

	if s.foreground == nil {
		s.foreground = make(chan struct{}, 1)
	}

	return s.foreground
}

// takeForeground gives the caller exclusive ownership of the session's foreground
// and returns the release. A prompt holds it from before its native dispatch
// until after its own settlement, and the reader holds it for each between-prompt
// frame it maps, so a prompt turn and an agent-origin turn can never be open at
// once and the handoff between them happens at one point rather than across a
// window.
//
// The prompt settles the excursion it pre-empts under this same ownership, which
// is what makes the handoff race-free in both directions: the excursion's turn is
// terminal before the prompt's is announced, and the reader's next frame finds
// the prompt's sink already published.
func (s *agentSession) takeForeground() func() {
	token := s.foregroundToken()
	token <- struct{}{}

	return func() { <-token }
}

// sessionToolUpdateOptions reports the session's tool-call mapping state. Native
// tool-use blocks and workflow tasks are named by the harness process rather than
// by the prompt that happened to be running, so the correlation follows the
// incarnation: a task started inside a prompt and completed after it is one task,
// and its later frames still resolve to the tool call the host already saw.
//
// The caller holds the session's foreground while it uses the result, which is
// what keeps this single-threaded mapping state single-threaded.
func (s *agentSession) sessionToolUpdateOptions() mapper.ToolUpdateOptions {
	if s.toolUpdates.Workflow == nil {
		s.resetSessionToolUpdateOptions()
	}

	return s.toolUpdates
}

// resetSessionToolUpdateOptions starts fresh correlation for a new native
// incarnation. The identities it holds are the retired process's, and the
// replacement process reuses none of them.
func (s *agentSession) resetSessionToolUpdateOptions() {
	s.toolUpdates = mapper.ToolUpdateOptions{
		Cwd:                    s.cwd,
		SupportsTerminalOutput: s.agent.clientSupportsTerminalOutput(),
		ToolUses:               make(map[string]claude.ToolUseBlock),
		Workflow:               mapper.NewWorkflowTracker(),
	}
}

// observeAutonomousFrame maps one native frame that arrived with no prompt owning
// the foreground, under the foreground token the caller already holds. A prompt
// publishes its sink under that same token, so a frame reaching here is a frame
// no prompt owns and the reader never has to offer it twice.
//
// A frame this adapter could not report is not dropped. This session advertises
// updatesOutsidePrompt, so a between-prompt frame the host never received is a
// hole in the only stream that describes it, and the incarnation that produced it
// is contained rather than left running behind a host that cannot see it.
func (s *agentSession) observeAutonomousFrame(
	ctx context.Context,
	incarnation *nativeIncarnation,
	msg claude.Message,
) {
	if incarnation == nil || incarnation.failed.Load() {
		// The incarnation this frame belongs to is already contained and the host
		// has already been told it ended. Ordinary content may not continue on a
		// lifecycle stream that is over.
		return
	}

	// The mapping is detached from the reader's context and bounded on its own.
	// A frame this session already read is work the host is owed whatever happens
	// to the reader behind it, and a host that stops reading must not be able to
	// wedge the incarnation's teardown behind an emission that never returns.
	mapCtx, cancel := settlementContext(ctx)
	defer cancel()

	if err := s.mapAutonomousFrame(mapCtx, msg); err != nil {
		s.failAutonomousFrame(ctx, incarnation, err)

		return
	}
}

// failAutonomousFrame contains the incarnation whose between-prompt frame this
// adapter could not report: the mapping, an ordinary ACP emission, a lifecycle
// event, or the durable prefix behind the excursion's own settlement. All four
// mean the same thing — the host's projection of this incarnation has a hole in
// it — so all four end the incarnation rather than being logged past.
//
// The latch names the exact native client and the containment carries its pump
// generation, so it refuses further work on that incarnation alone and a
// relaunched one starts clean.
//
// The containment itself runs behind this call. The caller is the native reader,
// and the retirement waits for that reader to stop: a reader that waited for its
// own stop would wedge the containment it is being contained by. The failed
// latch stops later callback registrations immediately. Existing host requests
// are cancelled only after the detached path proves their exact incarnation is
// still the one being served.
func (s *agentSession) failAutonomousFrame(
	ctx context.Context,
	incarnation *nativeIncarnation,
	cause error,
) {
	s.failNativeIncarnation(ctx, incarnation, cause, "projection")
}

func (s *agentSession) failNativeIncarnation(
	ctx context.Context,
	incarnation *nativeIncarnation,
	cause error,
	classification string,
) {
	if incarnation == nil || cause == nil {
		return
	}

	s.callbackOwnershipMu.Lock()
	failed := incarnation.failed.CompareAndSwap(false, true)
	s.callbackOwnershipMu.Unlock()

	if failed {
		// The session-level refusal is exact too. If a replacement was installed
		// before this failure reached the latch, A's failure names no work on B and
		// changes no session admission state.
		s.mu.Lock()
		if s.client == incarnation.client && s.autonomousErr == nil {
			s.autonomousErr = cause
			s.autonomousClient = incarnation.client
		}
		s.mu.Unlock()

		s.agent.log.ErrorContext(ctx, "contain failed Claude incarnation",
			slog.String(acpFieldSessionID, string(s.id)),
			slog.Uint64("native_generation", incarnation.generation),
			slog.String("classification", classification),
		)
	}

	detached := context.WithoutCancel(ctx)

	incarnation.superviseOnce.Do(func() {
		finishProducer, admitted := s.producers.begin()
		if !admitted {
			return
		}

		go func() {
			defer finishProducer()
			defer recoverAgentGoroutine(detached, s.agent.log, "session autonomous containment")
			// The exact identity check, reader retirement and lifecycle retirement are
			// serialized with serve. If B won first, this path may still close A but it
			// cannot stop B's reader, retire B's stream, clear B's route, or emit an idle
			// against B.
			s.pumpServeMu.Lock()
			defer s.pumpServeMu.Unlock()

			if !s.nativePumpHandle().serves(incarnation) {
				_ = incarnation.client.Close()

				return
			}

			_, err := s.endExactNativeIncarnationLocked(detached, incarnation)
			if err != nil {
				s.agent.log.DebugContext(detached, "Claude incarnation retirement failed",
					slog.Uint64("native_generation", incarnation.generation))
			}
		}()
	})
}

// autonomousFailureError reports the refusal a caller gets while the incarnation
// a between-prompt failure contained is still the one this session holds. A
// relaunch replaces that client, and the refusal names the contained incarnation
// alone rather than the session.
func (s *agentSession) autonomousFailureError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.autonomousErr == nil || s.autonomousClient != s.client {
		return nil
	}

	return autonomousStreamError(s.autonomousErr)
}

// clearAutonomousFailure releases the latch a retired incarnation left behind.
// It runs when a replacement incarnation is served: the failure described a
// process that no longer exists, and the new one owes the host nothing it failed
// to deliver.
func (s *agentSession) clearAutonomousFailure(current *claude.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.autonomousClient == current {
		return
	}

	s.autonomousErr = nil
	s.autonomousClient = nil
}

// autonomousStreamError is the uniform refusal for work addressed to an
// incarnation this adapter had to contain because it could not report that
// incarnation's own between-prompt output.
func autonomousStreamError(cause error) error {
	if cause == nil {
		return nil
	}

	return acp.NewInternalError(map[string]any{
		jsonFieldError:   "claude_autonomous_stream_failed",
		jsonFieldMessage: "the native incarnation could not be projected",
	})
}

// mapAutonomousFrame is the between-prompt half of the prompt loop. It reports
// the same content in the same order — the session's side effects, hook
// responses, and mapped updates — with
// the agent-origin turn that owns them opened ahead of the first frame that
// carries any.
func (s *agentSession) mapAutonomousFrame(ctx context.Context, msg claude.Message) error {
	options := s.sessionToolUpdateOptions()

	result, isResult := msg.(*claude.ResultMessage)

	var updates []acp.SessionUpdate

	if !isResult {
		updates = mapper.MessageToUpdatesWithOptions(msg, options)
		s.recordWorkflowFrameErrors(ctx, options.Workflow)
	}

	if s.excursion == nil {
		s.adoptAnnouncedExcursion()
	}

	if s.excursion == nil {
		// A frame carrying nothing the host is owed names no excursion, and a
		// result with no excursion open ends nothing: the turn that owned the work
		// it reports has already settled.
		if isResult || !autonomousFrameCarriesWork(msg, updates) {
			return nil
		}

		if err := s.openExcursion(ctx); err != nil {
			return err
		}
	}

	if isResult {
		return s.settleExcursion(ctx, result, options)
	}

	if err := s.observeExcursionFrame(ctx, msg); err != nil {
		return err
	}

	if err := s.emitMessageSideEffects(ctx, msg); err != nil {
		return err
	}

	if err := s.emitHookResponseUpdates(ctx, msg, options); err != nil {
		return err
	}

	// The frame's own content is reported before the terminal event it also
	// carries. A state frame that idles the harness still maps to updates, and a
	// terminal idle emitted ahead of them would state that the turn was over
	// while the host was still owed what that turn produced.
	if err := s.emitUpdates(ctx, updates); err != nil {
		return err
	}

	if promptFinishedBySystemIdle(msg) {
		return s.settleExcursion(ctx, nil, options)
	}

	return nil
}

// mapCausalBackgroundFrame projects a task frame while another prompt owns the
// foreground. It deliberately performs no foreground transition: the task is
// background work rooted in its original prompt, not work the current prompt
// may adopt or a second simultaneous foreground turn.
func (s *agentSession) mapCausalBackgroundFrame(ctx context.Context, msg claude.Message) error {
	options := s.sessionToolUpdateOptions()

	if result, ok := msg.(*claude.ResultMessage); ok {
		return s.emitUpdates(ctx, s.resultUsageUpdates(result, nil, ""))
	}

	if err := s.emitMessageSideEffects(ctx, msg); err != nil {
		return err
	}

	if err := s.emitHookResponseUpdates(ctx, msg, options); err != nil {
		return err
	}

	updates := mapper.MessageToUpdatesWithOptions(msg, options)
	s.recordWorkflowFrameErrors(ctx, options.Workflow)

	return s.emitUpdates(ctx, updates)
}

// autonomousFrameCarriesWork reports whether one between-prompt frame carries
// something the host is owed. A frame that maps to no update and triggers no
// session side effect is a frame with nothing in it, and a
// turn opened for it would report an excursion that never happened.
func autonomousFrameCarriesWork(
	msg claude.Message,
	updates []acp.SessionUpdate,
) bool {
	if len(updates) > 0 {
		return true
	}

	if stream, ok := msg.(*claude.StreamEventMessage); ok {
		_, usageKnown := streamUsageSnapshot(stream, usageSnapshot{}, false)

		return usageKnown
	}

	system, ok := msg.(*claude.SystemMessage)
	if !ok {
		return false
	}

	switch system.Subtype {
	case systemStatus, systemSubtypeCompactBoundary, systemSubtypeLocalCommandOutput,
		systemSubtypeHookResponse, elicitationComplete:
		return true
	default:
		return false
	}
}

// adoptAnnouncedExcursion takes over an agent-origin turn a control callback
// opened before the first frame of the same excursion arrived. Background work
// that asks permission before it produces any output is one excursion, so it is
// one turn, and the frames that follow belong to the turn already open rather
// than to a second one.
func (s *agentSession) adoptAnnouncedExcursion() {
	turnID := s.lifecycleStream().agentTurnID()
	if turnID == "" {
		return
	}

	s.excursion = &agentExcursion{turnID: turnID}
}

// openExcursion opens the agent-origin turn for one between-prompt run of native
// work. A connection that negotiated no lifecycle answer opens no turn and still
// streams the content: the excursion is a fact about the session either way, and
// only its lifecycle representation is negotiated.
func (s *agentSession) openExcursion(ctx context.Context) error {
	turnID, err := s.lifecycleStream().openAgentTurn(ctx, s.autonomousRoute())
	if err != nil {
		return err
	}

	s.excursion = &agentExcursion{turnID: turnID}

	return nil
}

// observeExcursionFrame accumulates the same per-turn state a prompt accumulates:
// the assistant identity the turn finishes on, the model the harness reported,
// and the streamed usage the host is shown as it arrives.
func (s *agentSession) observeExcursionFrame(ctx context.Context, msg claude.Message) error {
	excursion := s.excursion

	if assistant, ok := msg.(*claude.AssistantMessage); ok && assistant.ParentToolUseID == "" {
		if assistant.Model != "" && assistant.Model != syntheticModelName {
			excursion.lastAssistantModel = assistant.Model
		}

		if assistant.MessageID != "" {
			excursion.lastAssistantMessageID = assistant.MessageID
		}
	}

	stream, ok := msg.(*claude.StreamEventMessage)
	if !ok || stream.ParentToolUseID != "" {
		return nil
	}

	if model := streamModel(stream); model != "" && model != syntheticModelName {
		excursion.lastAssistantModel = model
	}

	updates, usage, known, total := s.streamUsageUpdates(
		stream, excursion.stream, excursion.streamKnown, excursion.streamEmittedTotal)
	if known {
		excursion.stream = usage
		excursion.streamKnown = true
		excursion.streamEmittedTotal = total
	}

	return s.emitUpdates(ctx, updates)
}

// settleExcursion ends the open agent-origin turn exactly once. The turn's
// recorded outcome is read from the same native result the content was, so a
// provider failure the harness reported ends the turn as failed rather than as a
// success nothing stands behind.
//
// The settlement runs in the order the contract fixes for every cycle this
// adapter closes: the native terminal has already arrived, the durable prefix
// commits next, and only what the store provably holds is what the terminal idle
// reports. A commit the store refused leaves the incarnation unsettled rather
// than announcing an end the session cannot stand behind.
func (s *agentSession) settleExcursion(
	ctx context.Context,
	result *claude.ResultMessage,
	options mapper.ToolUpdateOptions,
) error {
	excursion := s.excursion
	if excursion == nil {
		return nil
	}

	if result != nil {
		if err := s.emitUpdates(
			ctx, s.resultUsageUpdates(result, nil, excursion.lastAssistantModel),
		); err != nil {
			return err
		}

		if resultOriginKind(result) == originKindTaskNotification {
			return nil
		}
	}

	s.excursion = nil

	response := acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	failure := error(nil)

	if result != nil {
		response.StopReason = mapper.StopReason(result, false)
		failure = providerTurnFailure(result)
	}

	if failure == nil {
		if err := s.emitNativeMessageIdentity(ctx, excursion.lastAssistantMessageID); err != nil {
			return err
		}
	}

	if commitErr := s.commitSessionMirror(); commitErr != nil {
		s.lifecycleStream().abandonIncarnation()

		return storeCommitError(commitErr)
	}

	return s.lifecycleStream().settleTurn(
		ctx, excursion.turnID, lifecycleOutcomeFor(response, failure))
}

// excursionConflict reports the refusal a prompt gets while an agent-origin
// foreground turn is actually open. Background activity being live is no reason
// to refuse anything — a task running under a tool call is not a foreground —
// but an open excursion is a turn holding the one foreground this session has,
// and it may own a permission or an elicitation the host has not answered yet.
//
// Nothing is pre-empted. Cancelling that turn would report a cycle as ended while
// the native work behind it runs on, and terminalizing its pending action would
// answer a request nobody answered. The excursion stays owned and live, its
// native terminal settles it, and the retry proceeds against an idle foreground.
//
// It runs under the session's foreground and before the turn is attached or
// dispatched, so a refused prompt leaves no acceptance, no sink, and no native
// frame behind it.
func (s *agentSession) excursionConflict() error {
	if s.excursion == nil {
		s.adoptAnnouncedExcursion()
	}

	if s.excursion == nil {
		return nil
	}

	return backpressureError("session_foreground")
}
