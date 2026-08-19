package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

var finishPromptResultCall = (*agentSession).finishPromptResult

// Prompt sends one turn to Claude and streams updates.
func (s *agentSession) Prompt( //nolint:gocyclo // Turn admission and its single settlement remain visibly paired.
	ctx context.Context,
	params acp.PromptRequest,
) (response acp.PromptResponse, promptErr error) {
	route, err := parseInboundTurnRoute(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	// Route validation runs first, so a prompt never reports two rejections and
	// the order of two failures is never implementation-defined. The submission
	// identity is read next and before every native side effect: a correlation
	// value this adapter refuses writes no frame to the harness, and the turn the
	// route authenticated is the turn the acceptance names because the acceptance
	// is minted from that same validated route.
	submission, err := s.agent.readPromptCorrelation(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if poisonErr := s.poisonedError(); poisonErr != nil {
		return acp.PromptResponse{}, poisonErr
	}

	// A session that is already closing is refused before it is admitted, so a
	// caller gets the terminal answer rather than a native failure from the
	// process being torn down. The section that publishes the turn holds the
	// authoritative check.
	if s.isClosing() {
		return acp.PromptResponse{}, closedSessionError()
	}

	availableCommands := s.commands()
	commandName := mapper.PromptCommandName(params.Prompt)
	deniedName, alternative, denied := mapper.DeniedPromptCommand(params.Prompt, availableCommands)
	commandTurn := denied || (commandName != "" && s.commandAdvertised(commandName))

	releaseTurn, err := s.acquirePromptTurn(ctx, commandTurn)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer releaseTurn()

	if poisonErr := s.poisonedError(); poisonErr != nil {
		return acp.PromptResponse{}, poisonErr
	}

	if denied {
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{
			jsonFieldError:   "unsupported command",
			jsonFieldCommand: deniedName,
			"alternative":    alternative,
			jsonFieldMessage: "Use " + alternative + " instead of /" + deniedName + ".",
		})
	}

	localOnlyCommand := localOnlySlashCommand(params.Prompt)
	prompt := params.Prompt

	s.mu.Lock()
	advertisedCommands := cloneAvailableCommands(s.advertisedCommands)
	s.mu.Unlock()

	content, err := mapper.PromptToClaude(ctx, prompt, advertisedCommands, mapper.ImageInputLimits{
		MaxBytesPerImage:  s.agent.options.ImageLimits.MaxInputBytesPerImage,
		MaxBytesPerPrompt: s.agent.options.ImageLimits.MaxInputBytesPerPrompt,
	}, newHandoffImageReader(s.agent.options.InputHandoffRoot))
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if refreshErr := s.refreshMCPRegistry(ctx); refreshErr != nil {
		return acp.PromptResponse{}, nativeTurnFailure(refreshErr)
	}

	if clientErr := s.ensureClientAlive(ctx); clientErr != nil {
		return acp.PromptResponse{}, nativeTurnFailure(clientErr)
	}

	// The MCP relaunch and the lazy relaunch above both replace the native
	// process, so the reader and the incarnation identity are pointed at the
	// current one here: after validation and before acceptance.
	if pumpErr := s.serveNativePump(ctx, s.currentClient()); pumpErr != nil {
		return acp.PromptResponse{}, pumpErr
	}

	stream := s.lifecycleStream()
	turnID := ""

	var timedOut atomic.Bool

	s.cancelMu.Lock()
	s.resetPublishedToolCalls()
	s.mu.Lock()

	// This is the section that publishes the turn, so it is the one that has to
	// see the close: a check anywhere earlier only moves the race.
	if s.closing {
		s.mu.Unlock()
		s.cancelMu.Unlock()

		return acp.PromptResponse{}, closedSessionError()
	}

	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, route.turnNonce)
	s.cancel = cancel
	s.turnCancelled = false
	s.turnContainmentErr = nil
	s.turnNonce = route.turnNonce
	s.cancelledNonce = ""
	s.mu.Unlock()
	s.cancelMu.Unlock()

	defer func() {
		s.cancelMu.Lock()
		defer s.cancelMu.Unlock()

		// One settlement runs on every exit, in the one order the contract fixes:
		// the native terminal, then the containment boundary this configuration
		// selects, then the durable foreground-prefix commit, then the terminal
		// idle, then the response.
		response, promptErr = s.settlePromptTurn(
			ctx,
			turnCtx,
			params.MessageId,
			timedOut.Load(),
			response,
			promptErr,
		)
		response, promptErr = s.settleTurnLifecycle(ctx, stream, turnID, response, promptErr)

		s.mu.Lock()
		s.cancel = nil
		s.turnCancelled = false
		s.turnContainmentErr = nil
		s.turnNonce = ""
		s.cancelledNonce = ""
		s.mu.Unlock()

		cancel()
	}()

	if timeout := s.agent.turnTimeout(); timeout > 0 {
		timeoutDone := make(chan struct{})

		timer := time.AfterFunc(timeout, func() {
			defer close(timeoutDone)

			timedOut.Store(true)
			cancel()
		})
		defer func() {
			if !timer.Stop() {
				<-timeoutDone
			}
		}()
	}

	defer s.client.EndQuery(route.turnNonce)

	// The turn is attached to the session's reader before the frame is dispatched,
	// so no frame this turn caused can arrive before there is somewhere for it to
	// go.
	sink, releaseSink := s.nativePumpHandle().attachTurn()
	defer releaseSink()

	var (
		sendErr    error
		dispatched bool
	)

	turnID, err = stream.dispatch(ctx, submission, route.turnNonce, func() error {
		sendErr = s.client.Query(turnCtx, route.turnNonce, content)
		dispatched = sendErr == nil

		return sendErr
	})

	if sendErr != nil {
		return acp.PromptResponse{}, nativeTurnFailure(sendErr)
	}

	if err != nil {
		if dispatched {
			// The harness took the frame and the stream then failed to announce it, so
			// the host has no lifecycle event covering the turn that is now running.
			// It is contained before the failure returns: native work this adapter
			// cannot describe does not outlive the call that started it.
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}

		return acp.PromptResponse{}, err
	}

	toolUpdateOptions := mapper.ToolUpdateOptions{
		Cwd:                    s.cwd,
		SupportsTerminalOutput: s.agent.clientSupportsTerminalOutput(),
		ToolUses:               make(map[string]claude.ToolUseBlock),
		Workflow:               mapper.NewWorkflowTracker(),
	}

	state := &promptLoopState{}

	for {
		msg, err := s.nativePumpHandle().next(turnCtx, sink)
		if err != nil {
			if poisonErr := s.poisonedError(); poisonErr != nil {
				return acp.PromptResponse{}, poisonErr
			}

			return s.receiveTurnFailure(ctx, turnCtx, params.MessageId, err, timedOut.Load())
		}

		if err := s.observePromptMessage(turnCtx, msg, state); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}

		if result, ok := msg.(*claude.ResultMessage); ok {
			resp, done, err := finishPromptResultCall(
				s,
				turnCtx,
				ctx,
				params,
				result,
				state,
				toolUpdateOptions,
				localOnlyCommand,
			)
			if err != nil {
				return acp.PromptResponse{}, err
			}

			if !done {
				continue
			}

			if err := s.refreshCommandsAfterPromptCommand(turnCtx, commandName); err != nil {
				return acp.PromptResponse{}, err
			}

			return resp, nil
		}

		if err := s.emitMessageSideEffects(turnCtx, msg); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}

		if err := s.emitHookResponseUpdates(turnCtx, msg, toolUpdateOptions); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}

		if promptFinishedBySystemIdle(msg) {
			return s.finishPromptSystemIdle(turnCtx, ctx, params, state, commandName)
		}

		updates := mapper.MessageToUpdatesWithOptions(msg, toolUpdateOptions)
		s.recordWorkflowFrameErrors(turnCtx, toolUpdateOptions.Workflow)

		if err := s.emitUpdates(turnCtx, updates); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}
	}
}

// finishPromptSystemIdle ends a turn the harness closed with a state frame rather
// than a result frame. There is no result to read, so there is no native stop
// reason, no usage beyond what the turn already streamed, and no provider error
// to classify: end_turn is what the harness actually reported, and the lifecycle
// outcome is derived from this same response so the two can never disagree.
func (s *agentSession) finishPromptSystemIdle(
	turnCtx context.Context,
	interruptCtx context.Context,
	params acp.PromptRequest,
	state *promptLoopState,
	commandName string,
) (acp.PromptResponse, error) {
	stopReason := acp.StopReasonEndTurn
	if s.wasTurnCancelled() {
		stopReason = acp.StopReasonCancelled
	}

	if err := s.emitCompletedNativeMessageIdentity(
		turnCtx, state.lastAssistantMessageID, stopReason == acp.StopReasonCancelled,
	); err != nil {
		return acp.PromptResponse{}, s.interruptAfterEmitError(interruptCtx, err)
	}

	if err := s.emitLiveSessionInfoUpdate(turnCtx, params.Prompt); err != nil {
		return acp.PromptResponse{}, s.interruptAfterEmitError(interruptCtx, err)
	}

	if err := s.refreshCommandsAfterPromptCommand(turnCtx, commandName); err != nil {
		return acp.PromptResponse{}, s.interruptAfterEmitError(interruptCtx, err)
	}

	return acp.PromptResponse{
		Meta:          assistantIdentityMeta(state.lastAssistantMessageID),
		StopReason:    stopReason,
		Usage:         state.promptUsage,
		UserMessageId: params.MessageId,
	}, nil
}

func (s *agentSession) acquirePromptTurn(ctx context.Context, exclusive bool) (func(), error) {
	if exclusive {
		return s.acquireExclusiveTurn(ctx)
	}

	return s.acquireTurn(ctx)
}

func (s *agentSession) commandAdvertised(name string) bool {
	if name == "" {
		return false
	}

	commands := availableCommandsFromUpdates(mapper.AvailableCommandsUpdate(s.commands()))
	for _, command := range commands {
		if command.Name == name {
			return true
		}
	}

	return false
}

func (s *agentSession) refreshCommandsAfterPromptCommand(ctx context.Context, name string) error {
	if !commandRefreshesAvailableCommands(name) || !s.commandAdvertised(name) {
		return nil
	}

	info, err := s.client.RefreshInitializeInfo(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.availableCommands = info.Commands
	s.mu.Unlock()

	return s.emitAvailableCommandsUpdate(ctx, false)
}

func commandRefreshesAvailableCommands(name string) bool {
	return name == commandReloadSkills || name == commandReloadPlugins
}

func (s *agentSession) finishPromptResult(
	turnCtx context.Context,
	interruptCtx context.Context,
	params acp.PromptRequest,
	result *claude.ResultMessage,
	state *promptLoopState,
	toolUpdateOptions mapper.ToolUpdateOptions,
	localOnlyCommand bool,
) (acp.PromptResponse, bool, error) {
	state.promptUsage = mergeUsage(state.promptUsage, mapper.Usage(result))

	contextUsage, contextUsageErr := s.client.GetContextUsage(turnCtx)
	if contextUsageErr != nil {
		s.agent.log.DebugContext(turnCtx, "get Claude context usage failed", slog.String(jsonFieldError, contextUsageErr.Error()))
	}

	if err := s.emitOptionalUpdates(
		turnCtx,
		s.resultUsageUpdates(result, contextUsage, state.lastAssistantModel),
	); err != nil {
		return acp.PromptResponse{}, false, s.interruptAfterEmitError(interruptCtx, err)
	}

	if resultOriginKind(result) == originKindTaskNotification &&
		!workflowTaskNotificationResultCompletesPrompt(toolUpdateOptions.Workflow) {
		return acp.PromptResponse{}, false, nil
	}

	cancelled := s.wasTurnCancelled()
	if !cancelled {
		if err := providerTurnFailure(result); err != nil {
			return acp.PromptResponse{}, false, err
		}
	}

	if !cancelled && localOnlyCommand && strings.TrimSpace(result.Result) != "" {
		if err := s.emitUpdates(turnCtx, []acp.SessionUpdate{acp.UpdateAgentMessageText(result.Result)}); err != nil {
			return acp.PromptResponse{}, false, s.interruptAfterEmitError(interruptCtx, err)
		}
	}

	if err := s.emitCompletedNativeMessageIdentity(
		turnCtx, state.lastAssistantMessageID, cancelled,
	); err != nil {
		return acp.PromptResponse{}, false, s.interruptAfterEmitError(interruptCtx, err)
	}

	if err := s.emitLiveSessionInfoUpdate(turnCtx, params.Prompt); err != nil {
		return acp.PromptResponse{}, false, s.interruptAfterEmitError(interruptCtx, err)
	}

	s.logUnknownStopReason(turnCtx, result)

	return acp.PromptResponse{
		Meta:          assistantIdentityMeta(state.lastAssistantMessageID),
		StopReason:    mapper.StopReason(result, cancelled),
		Usage:         state.promptUsage,
		UserMessageId: params.MessageId,
	}, true, nil
}

func workflowTaskNotificationResultCompletesPrompt(tracker *mapper.WorkflowTracker) bool {
	if tracker == nil {
		return true
	}

	return tracker.HasTracked() && !tracker.HasActive()
}

func (s *agentSession) logUnknownStopReason(ctx context.Context, result *claude.ResultMessage) {
	if reason := mapper.UnknownStopReason(result); reason != "" {
		s.agent.log.DebugContext(ctx, "unknown Claude stop reason", slog.String("stop_reason", reason))
	}
}

// appendSessionMirror writes one transcript mirror frame to the store. It runs on
// the session's ordered outbox, under a context detached from every turn and
// every request, so a cancel can never abort a write that is already in flight or
// leave a retry half done.
func (s *agentSession) appendSessionMirror(ctx context.Context, frame *claude.TranscriptMirrorMessage) error {
	if s.mirror == nil || s.mirror.store == nil || len(frame.Entries) == 0 {
		return nil
	}

	ctx, finishAppend := s.agent.observe.StartSessionStore(ctx, "append")
	err := s.mirror.appendFrame(ctx, frame)
	finishAppend(err)

	return err
}

// settleTurnLifecycle is the turn's one durability and lifecycle boundary, and it
// runs on every exit. The containment boundary this configuration selects has
// already completed when it starts, so it commits the native-safe foreground
// prefix, and only once that prefix is durable does the terminal idle report the
// outcome the response carries.
//
// Durability outranks the terminal event. Where the containment boundary or the
// commit itself failed, the prompt fails with its own error, no terminal idle is
// emitted at all, and the incarnation ends unsettled; the next incarnation's
// snapshot asserts the truthful state.
//
// The request context reaches here only for its values. Settlement states what
// the turn already did, and each emission below detaches from the caller's
// cancellation itself, so a host that withdraws its prompt cannot hole the stream
// over work that completed anyway.
func (s *agentSession) settleTurnLifecycle(
	ctx context.Context,
	stream *sessionStream,
	turnID string,
	response acp.PromptResponse,
	promptErr error,
) (acp.PromptResponse, error) {
	if s.turnContainmentError() != nil {
		stream.abandonIncarnation()

		return response, promptErr
	}

	if commitErr := s.commitSessionMirror(); commitErr != nil {
		stream.abandonIncarnation()

		if promptErr != nil {
			return response, promptErr
		}

		return acp.PromptResponse{}, storeCommitError(commitErr)
	}

	if err := stream.settleTurn(ctx, turnID, lifecycleOutcomeFor(response, promptErr)); err != nil {
		return acp.PromptResponse{}, err
	}

	if !s.nativePumpHandle().incarnationEnded() {
		return response, promptErr
	}

	// The turn ended with no process behind it — a cancel that closed the client,
	// a native exit, or a timeout that contained it. That is the end of the
	// incarnation, and the next prompt's relaunch opens a new one.
	if err := stream.loseIncarnation(ctx); err != nil {
		return acp.PromptResponse{}, err
	}

	return response, promptErr
}

// storeCommitError is what a turn returns when the store does not hold what that
// turn streamed. It is not a native turn failure: the harness produced the turn
// correctly and the durability boundary that failed is the adapter's own.
func storeCommitError(err error) error {
	return acp.NewInternalError(map[string]any{
		jsonFieldError:   "claude_store_commit_failed",
		jsonFieldMessage: err.Error(),
	})
}

// turnContainmentError reports the selected containment boundary's failure for
// the turn that just settled.
func (s *agentSession) turnContainmentError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnContainmentErr
}

// currentClient reports the session's native client.
func (s *agentSession) currentClient() *claude.Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.client
}

func (s *agentSession) wasTurnCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.turnCancelled
}

func (s *agentSession) observePromptMessage(ctx context.Context, msg claude.Message, state *promptLoopState) error {
	if assistant, ok := msg.(*claude.AssistantMessage); ok {
		observeAssistantMessage(assistant, state)
	}

	stream, ok := msg.(*claude.StreamEventMessage)
	if !ok || stream.ParentToolUseID != "" {
		return nil
	}

	model := streamModel(stream)

	updates, usage, usageKnown, total := s.streamUsageUpdates(
		stream,
		state.lastStreamUsage,
		state.lastStreamUsageKnown,
		state.lastEmittedUsageTotal,
	)
	if usageKnown {
		state.lastStreamUsage = usage
		state.lastStreamUsageKnown = true
		state.lastEmittedUsageTotal = total
	}

	if model != "" && model != syntheticModelName {
		state.lastAssistantModel = model
	}

	return s.emitOptionalUpdates(ctx, updates)
}

func (s *agentSession) recordWorkflowFrameErrors(ctx context.Context, tracker *mapper.WorkflowTracker) {
	if tracker == nil || s.agent == nil {
		return
	}

	for _, err := range tracker.DrainFrameErrors() {
		s.agent.observe.RecordWorkflowFrameError(ctx, err.Outcome, err.ErrorType, err.FrameSubtype)
	}
}

func observeAssistantMessage(assistant *claude.AssistantMessage, state *promptLoopState) {
	if assistant.ParentToolUseID != "" {
		return
	}

	if assistant.Model != "" && assistant.Model != syntheticModelName {
		state.lastAssistantModel = assistant.Model
	}

	if assistant.MessageID != "" {
		state.lastAssistantMessageID = assistant.MessageID
	}
}

func assistantIdentityMeta(messageID string) map[string]any {
	if messageID == "" {
		return nil
	}

	return map[string]any{
		claudeMetaKey: map[string]any{
			jsonFieldMessageID: messageID,
		},
	}
}

func (s *agentSession) emitCompletedNativeMessageIdentity(
	ctx context.Context,
	messageID string,
	cancelled bool,
) error {
	if cancelled {
		return nil
	}

	return s.emitNativeMessageIdentity(ctx, messageID)
}

func localOnlySlashCommand(prompt []acp.ContentBlock) bool {
	switch mapper.PromptCommandName(prompt) {
	case localCommandContext[1:], localCommandExtraUsage[1:], localCommandHeapdump[1:]:
		return true
	default:
		return false
	}
}

func firstPromptText(prompt []acp.ContentBlock) string {
	for _, block := range prompt {
		if block.Text == nil {
			continue
		}

		return block.Text.Text
	}

	return ""
}

func firstPromptToken(text string) string {
	name := mapper.PromptCommandName([]acp.ContentBlock{acp.TextBlock(text)})
	if name == "" {
		return ""
	}

	return "/" + name
}

func (s *agentSession) streamUsageUpdates(
	msg *claude.StreamEventMessage,
	previous usageSnapshot,
	previousKnown bool,
	lastEmittedTotal int,
) ([]acp.SessionUpdate, usageSnapshot, bool, int) {
	next, ok := streamUsageSnapshot(msg, previous, previousKnown)
	if !ok {
		return nil, previous, previousKnown, lastEmittedTotal
	}

	total := next.total()
	if previousKnown && total == lastEmittedTotal {
		return nil, next, true, lastEmittedTotal
	}

	return []acp.SessionUpdate{{
		UsageUpdate: &acp.SessionUsageUpdate{
			Meta: streamUsageMeta(next),
			Size: s.currentContextWindow(),
			Used: total,
		},
	}}, next, true, total
}

func streamUsageSnapshot(
	msg *claude.StreamEventMessage,
	previous usageSnapshot,
	previousKnown bool,
) (usageSnapshot, bool) {
	switch msg.EventType {
	case streamEventMessageStart:
		message := mapValue(msg.Event["message"])

		usage := mapValue(message["usage"])
		if usage == nil {
			return usageSnapshot{}, false
		}

		return usageSnapshotFromMap(usage), true
	case streamEventMessageDelta:
		usage := mapValue(msg.Event["usage"])
		if usage == nil {
			return previous, false
		}

		if !previousKnown {
			previous = usageSnapshot{}
		}

		return previous.patch(usage), true
	default:
		return previous, false
	}
}

func streamModel(msg *claude.StreamEventMessage) string {
	if msg == nil || msg.EventType != streamEventMessageStart {
		return ""
	}

	message := mapValue(msg.Event["message"])

	return stringValue(message["model"])
}

func usageSnapshotFromMap(raw map[string]any) usageSnapshot {
	return usageSnapshot{
		inputTokens:          intValue(raw["input_tokens"]),
		outputTokens:         intValue(raw["output_tokens"]),
		cacheReadTokens:      intValue(raw["cache_read_input_tokens"]),
		cacheCreationTokens:  intValue(raw["cache_creation_input_tokens"]),
		reasoningOutputToken: intValue(raw["reasoning_output_tokens"]),
	}
}

func (u usageSnapshot) patch(raw map[string]any) usageSnapshot {
	if value, ok := intField(raw, "input_tokens"); ok {
		u.inputTokens = value
	}

	if value, ok := intField(raw, "output_tokens"); ok {
		u.outputTokens = value
	}

	if value, ok := intField(raw, "cache_read_input_tokens"); ok {
		u.cacheReadTokens = value
	}

	if value, ok := intField(raw, "cache_creation_input_tokens"); ok {
		u.cacheCreationTokens = value
	}

	if value, ok := intField(raw, "reasoning_output_tokens"); ok {
		u.reasoningOutputToken = value
	}

	return u
}

func (u usageSnapshot) total() int {
	return u.inputTokens + u.outputTokens + u.cacheReadTokens + u.cacheCreationTokens + u.reasoningOutputToken
}

func streamUsageMeta(usage usageSnapshot) map[string]any {
	acpUsage := &acp.Usage{
		InputTokens:  usage.inputTokens,
		OutputTokens: usage.outputTokens,
		TotalTokens:  usage.total(),
	}
	acpUsage.CachedReadTokens = acp.Ptr(usage.cacheReadTokens)
	acpUsage.CachedWriteTokens = acp.Ptr(usage.cacheCreationTokens)
	acpUsage.ThoughtTokens = acp.Ptr(usage.reasoningOutputToken)

	return map[string]any{
		claudeMetaKey: map[string]any{
			usageMetaKey: mapper.UsageMeta(acpUsage),
		},
	}
}

func (s *agentSession) resultUsageUpdates(
	result *claude.ResultMessage,
	contextUsage *claude.ContextUsage,
	model string,
) []acp.SessionUpdate {
	if result == nil {
		return nil
	}

	used := 0
	if usage := mapper.Usage(result); usage != nil {
		used = usage.TotalTokens
	}

	if contextUsage != nil && contextUsage.TotalTokens > 0 {
		used = contextUsage.TotalTokens
	}

	cost := (*acp.Cost)(nil)
	if result.TotalCostUSD != nil {
		cost = &acp.Cost{Amount: *result.TotalCostUSD, Currency: "USD"}
	}

	if used == 0 && cost == nil && len(result.StructuredOutput) == 0 {
		return nil
	}

	update := acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{
			Cost: cost,
			Size: s.updateContextWindow(result, contextUsage, model),
			Used: used,
		},
	}
	if usageMeta := mapper.ClaudeUsageMeta(result); len(usageMeta) > 0 {
		maps.Copy(sessionUsageClaudeMeta(update.UsageUpdate), usageMeta)
	}

	if len(result.Origin) > 0 {
		sessionUsageClaudeMeta(update.UsageUpdate)[rawMessageOriginKey] = result.Origin
	}

	if len(result.StructuredOutput) > 0 {
		sessionUsageClaudeMeta(update.UsageUpdate)[structuredOutputMetaKey] = result.StructuredOutput
	}

	return []acp.SessionUpdate{update}
}

func sessionUsageClaudeMeta(update *acp.SessionUsageUpdate) map[string]any {
	if update.Meta == nil {
		update.Meta = map[string]any{}
	}

	claudeMeta, _ := update.Meta[claudeMetaKey].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = map[string]any{}
		update.Meta[claudeMetaKey] = claudeMeta
	}

	return claudeMeta
}

func (s *agentSession) emitCurrentUsageUpdate(ctx context.Context) {
	contextUsage, err := s.client.GetContextUsage(ctx)
	if err != nil {
		s.agent.log.DebugContext(ctx, "get Claude context usage failed", slog.String(jsonFieldError, err.Error()))

		return
	}

	_ = s.emitOptionalUpdates(ctx, s.contextUsageUpdates(contextUsage))
}

func (s *agentSession) contextUsageUpdates(contextUsage *claude.ContextUsage) []acp.SessionUpdate {
	if contextUsage == nil || contextUsage.MaxTokens <= 0 {
		return nil
	}

	s.setContextWindowSize(contextUsage.MaxTokens)

	return []acp.SessionUpdate{{
		UsageUpdate: &acp.SessionUsageUpdate{
			Size: contextUsage.MaxTokens,
			Used: contextUsage.TotalTokens,
		},
	}}
}

func (s *agentSession) updateContextWindow(
	result *claude.ResultMessage,
	contextUsage *claude.ContextUsage,
	model string,
) int {
	size := s.currentContextWindow()
	authoritative := false

	if contextUsage != nil && contextUsage.MaxTokens > 0 {
		size = contextUsage.MaxTokens
		authoritative = true
	} else if usage, ok := matchingModelUsage(result.ModelUsage, model); ok && usage.ContextWindow > 0 {
		size = usage.ContextWindow
		authoritative = true
	}

	if authoritative {
		s.setContextWindowSize(size)
	}

	return size
}

func matchingModelUsage(usages map[string]claude.ModelUsage, model string) (claude.ModelUsage, bool) {
	if model == "" {
		return claude.ModelUsage{}, false
	}

	if usage, ok := usages[model]; ok {
		return usage, true
	}

	bestPrefix := 0

	var best claude.ModelUsage

	for name, usage := range usages {
		prefix := commonPrefixLength(name, model)
		if prefix > bestPrefix {
			bestPrefix = prefix
			best = usage
		}
	}

	return best, bestPrefix > 0
}

func commonPrefixLength(left string, right string) int {
	limit := min(len(left), len(right))
	for i := range limit {
		if left[i] != right[i] {
			return i
		}
	}

	return limit
}

// currentContextWindow returns the context window the Claude harness has
// reported for this session, or 0 when it is still unknown. It is never
// fabricated from a static model-name catalog, so usage_update.size is only ever
// a harness-reported value or 0.
func (s *agentSession) currentContextWindow() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.contextWindowSize
}

func (s *agentSession) setContextWindowSize(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.contextWindowSize = size
}

func mapValue(value any) map[string]any {
	typed, _ := value.(map[string]any)

	return typed
}

func stringValue(value any) string {
	typed, _ := value.(string)

	return typed
}

func intField(raw map[string]any, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}

	value, ok := raw[key]
	if !ok || value == nil {
		return 0, ok
	}

	return intValue(value), true
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func (s *agentSession) interruptAfterEmitError(ctx context.Context, err error) error {
	interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	cancelErr := s.Cancel(interruptCtx)
	if errors.Is(cancelErr, claude.ErrProcessContainmentIncomplete) {
		return errors.Join(err, cancelErr)
	}

	return err
}
