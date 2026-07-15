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
func (s *agentSession) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	route, err := parseInboundTurnRoute(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if poisonErr := s.poisonedError(); poisonErr != nil {
		return acp.PromptResponse{}, poisonErr
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
			"command":        deniedName,
			"alternative":    alternative,
			jsonFieldMessage: "Use " + alternative + " instead of /" + deniedName + ".",
		})
	}

	s.stopLateMirrorProcessor(ctx)

	localOnlyCommand := localOnlySlashCommand(params.Prompt)
	prompt := params.Prompt

	s.mu.Lock()
	advertisedCommands := cloneAvailableCommands(s.advertisedCommands)
	s.mu.Unlock()

	content, err := mapper.PromptToClaude(prompt, advertisedCommands)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.refreshMCPRegistry(ctx); err != nil {
		return acp.PromptResponse{}, nativeTurnFailure(err)
	}

	if err := s.ensureClientAlive(ctx); err != nil {
		return acp.PromptResponse{}, nativeTurnFailure(err)
	}

	s.cancelMu.Lock()
	s.mu.Lock()
	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, route.turnNonce)
	s.cancel = cancel
	s.turnCancelled = false
	s.turnNonce = route.turnNonce
	s.mu.Unlock()
	s.cancelMu.Unlock()

	defer func() {
		s.cancelMu.Lock()
		defer s.cancelMu.Unlock()

		s.mu.Lock()
		s.cancel = nil
		s.turnCancelled = false
		s.turnNonce = ""
		s.mu.Unlock()

		cancel()
	}()

	var timedOut atomic.Bool

	if timeout := s.agent.turnTimeout(); timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			cancel()
		})
		defer timer.Stop()
	}

	if err := s.client.Query(turnCtx, content); err != nil {
		return acp.PromptResponse{}, nativeTurnFailure(err)
	}

	toolUpdateOptions := mapper.ToolUpdateOptions{
		Cwd:                    s.cwd,
		SupportsTerminalOutput: s.agent.clientSupportsTerminalOutput(),
		ToolUses:               make(map[string]claude.ToolUseBlock),
	}

	state := &promptLoopState{}

	for {
		msg, err := s.client.Receive(turnCtx)
		if err != nil {
			return s.receiveTurnFailure(ctx, turnCtx, params.MessageId, err, timedOut.Load())
		}

		s.emitRawClaudeMessage(turnCtx, msg)

		if err := s.checkNativeSessionInvariant(turnCtx, msg); err != nil {
			return acp.PromptResponse{}, err
		}

		if handled, err := s.handleSessionMirror(turnCtx, msg); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		} else if handled {
			continue
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
			return s.finishPromptSystemIdle(turnCtx, ctx, params, state, toolUpdateOptions, commandName)
		}

		updates := mapper.MessageToUpdatesWithOptions(msg, toolUpdateOptions)
		s.recordWorkflowFrameErrors(turnCtx, toolUpdateOptions.Workflow)

		if err := s.emitUpdates(turnCtx, updates); err != nil {
			return acp.PromptResponse{}, s.interruptAfterEmitError(ctx, err)
		}
	}
}

func (s *agentSession) finishPromptSystemIdle(
	turnCtx context.Context,
	interruptCtx context.Context,
	params acp.PromptRequest,
	state *promptLoopState,
	toolUpdateOptions mapper.ToolUpdateOptions,
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

	if err := s.drainSessionMirror(turnCtx, toolUpdateOptions); err != nil {
		return acp.PromptResponse{}, s.interruptAfterEmitError(interruptCtx, err)
	}

	s.startLateMirrorProcessor(interruptCtx, toolUpdateOptions)

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

	if err := s.drainSessionMirror(turnCtx, toolUpdateOptions); err != nil {
		return acp.PromptResponse{}, false, s.interruptAfterEmitError(interruptCtx, err)
	}

	s.startLateMirrorProcessor(interruptCtx, toolUpdateOptions)

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

func (s *agentSession) handleSessionMirror(ctx context.Context, msg claude.Message) (bool, error) {
	if err := s.poisonedError(); err != nil {
		return false, err
	}

	frame, isMirror := msg.(*claude.TranscriptMirrorMessage)
	if !isMirror {
		return false, nil
	}

	if s.mirror.store == nil || len(frame.Entries) == 0 {
		return true, nil
	}

	ctx, finishAppend := s.agent.observe.StartSessionStore(ctx, "append")
	err := s.mirror.appendFrame(ctx, frame)
	finishAppend(err)

	return true, err
}

func (s *agentSession) drainSessionMirror(ctx context.Context, options ...mapper.ToolUpdateOptions) error {
	drainCtx, cancel := context.WithTimeout(ctx, sessionMirrorDrainTimeout)
	defer cancel()

	toolUpdateOptions := mapper.ToolUpdateOptions{}
	if len(options) > 0 {
		toolUpdateOptions = options[0]
	}

	for {
		msg, err := s.client.Receive(drainCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil
			}

			return err
		}

		s.emitRawClaudeMessage(ctx, msg)

		if err := s.checkNativeSessionInvariant(ctx, msg); err != nil {
			return err
		}

		if _, err := s.handleSessionMirror(ctx, msg); err != nil {
			return err
		}

		if toolUpdateOptions.Workflow == nil {
			continue
		}

		updates := mapper.MessageToUpdatesWithOptions(msg, toolUpdateOptions)
		s.recordWorkflowFrameErrors(ctx, toolUpdateOptions.Workflow)

		if err := s.emitUpdates(ctx, updates); err != nil {
			return err
		}
	}
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
			"messageId": messageID,
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

	_ = s.Cancel(interruptCtx)

	return err
}
