package claudeacp

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

// costCurrency is the currency of claude's total_cost_usd.
const costCurrency = "USD"

const (
	messageRoleUser          = "user"
	messageRoleAssistant     = "assistant"
	contentBlockTypeText     = "text"
	contentBlockTypeThinking = "thinking"
	contentBlockTypeToolCall = "tool_use"
	contentBlockTypeImage    = "image"
)

type messageState struct {
	id, text, thinking string
	// finalized and images hold the assistant rows and images already
	// emitted for the API message named by rows. A live stream delivers every
	// row of one API message before the next message begins, so they are
	// reset when another message arrives; replay keeps them for the whole
	// transcript.
	rows      string
	finalized map[string]struct{}
	images    map[string]struct{}
}

type cycleState struct {
	replay bool
	// usage is the turn's summed consumption as result reports it, for the
	// prompt response.
	usage            *acp.Usage
	cost             float64
	structuredOutput any
	stopReason       string
	errorMessage     string
	imagesEmitted    bool
	messages         map[string]*messageState
	tools            map[string]*toolState
	// context is the context the latest top-level model call occupied; 0
	// when no call has reported since the cycle opened or claude compacted.
	context int
	// call is the response id of the latest top-level model call, empty when
	// claude reported none, and callStart the usage its message_start
	// reported, empty when that report stated nothing; callOpen holds until
	// the call's message_delta reports its output.
	call      string
	callStart wire.CallUsage
	callOpen  bool
	// model is the latest top-level call's model, the result's modelUsage
	// entry that states its context window.
	model string
}

func (state *cycleState) message(parent string) *messageState {
	if state.messages == nil {
		state.messages = make(map[string]*messageState)
	}

	if state.messages[parent] == nil {
		state.messages[parent] = &messageState{finalized: make(map[string]struct{}), images: make(map[string]struct{})}
	}

	return state.messages[parent]
}

type parentToolKey struct{}

// emit preserves delegated provenance on every derived update.
func (s *session) emit(ctx context.Context, updates ...acp.SessionUpdate) error {
	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	parent, _ := ctx.Value(parentToolKey{}).(string)
	for _, update := range updates {
		if parent != "" {
			meta := map[string]any{vendor: map[string]any{"parentToolUseId": parent}}

			switch {
			case update.AgentMessageChunk != nil:
				update.AgentMessageChunk.Meta = meta
			case update.AgentThoughtChunk != nil:
				update.AgentThoughtChunk.Meta = meta
			case update.UserMessageChunk != nil:
				update.UserMessageChunk.Meta = meta
			case update.ToolCall != nil:
				update.ToolCall.Meta = meta
			case update.ToolCallUpdate != nil:
				update.ToolCallUpdate.Meta = meta
			}
		}

		if err := conn.SessionUpdate(context.WithoutCancel(ctx), acp.SessionNotification{SessionId: s.id, Update: update}); err != nil {
			return err
		}
	}

	return nil
}

// projectEvent maps native stream records, with result as the turn boundary.
func (s *session) projectEvent(ctx context.Context, _ *runtime, c *cycle, event claude.Event) (bool, error) {
	if event.Type == nativeSystem && event.Subtype == nativeCompactBoundary && event.ParentToolUseID == "" {
		c.state.context, c.state.call, c.state.callOpen = 0, "", false

		return false, s.projectCompaction(ctx, event)
	}

	if s.cycleCancelled(c) {
		return event.Type == nativeResult && event.ParentToolUseID == "", nil
	}

	ctx = context.WithValue(ctx, parentToolKey{}, event.ParentToolUseID)
	state := &c.state

	switch event.Type {
	case nativeStreamEvent:
		return false, s.projectStream(ctx, state, event)
	case messageRoleAssistant:
		if event.Message == nil {
			return false, nil
		}

		return false, s.projectAssistant(ctx, state, event.ParentToolUseID, event.UUID, *event.Message)
	case messageRoleUser:
		if event.Message == nil {
			return false, nil
		}

		blocks, err := event.Message.ContentBlocks()
		if err != nil {
			return false, err
		}

		return false, s.projectToolResults(ctx, state, blocks)
	case nativeResult:
		if event.ParentToolUseID != "" {
			return false, nil
		}

		state.stopReason = event.StopReason
		state.cost = event.TotalCostUSD
		state.structuredOutput = event.StructuredOutput

		state.usage = mapUsage(event.Usage)
		if event.IsError {
			state.stopReason = stopReasonError

			state.errorMessage = strings.Join(append(event.Errors, event.Error, event.Result), "\n")
			if strings.TrimSpace(state.errorMessage) == "" {
				state.errorMessage = event.Subtype
			}
		}

		if usage := event.ModelUsage[state.model]; usage.ContextWindow > 0 {
			s.mu.Lock()
			s.contextWindow = usage.ContextWindow
			s.mu.Unlock()
		}

		return true, nil
	case nativeSystem:
		return event.Subtype == "task_notification" && c.Origin != lifecycle.CauseSubmission, nil
	}

	return false, nil
}

func (s *session) projectStream(ctx context.Context, state *cycleState, event claude.Event) error {
	if event.Event == nil {
		return nil
	}

	native := event.Event
	message := state.message(event.ParentToolUseID)
	topLevel := event.ParentToolUseID == ""

	switch native.Type {
	case nativeMessageStart:
		message.id, message.text, message.thinking = "", "", ""
		if native.Message != nil {
			message.id = native.Message.ID

			if topLevel {
				return s.emitResponseInput(ctx, state, *native.Message)
			}
		}
	case nativeMessageDelta:
		if topLevel {
			return s.emitResponseUsage(ctx, state, native.Usage)
		}
	case "content_block_delta":
		switch native.Delta.Type {
		case "text_delta":
			message.text += native.Delta.Text

			return s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(native.Delta.Text), MessageId: optionalString(message.id)}})
		case "thinking_delta":
			message.thinking += native.Delta.Thinking

			return s.emit(ctx, acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(native.Delta.Thinking), MessageId: optionalString(message.id)}})
		}
	}

	return nil
}

func (s *session) projectAssistant(ctx context.Context, state *cycleState, parent string, rowID string, native claude.Message) error {
	message := state.message(parent)
	if !state.replay && native.ID != message.rows {
		message.rows = native.ID
		clear(message.finalized)
		clear(message.images)
	}

	if _, seen := message.finalized[rowID]; rowID != "" && seen {
		return nil
	}

	if rowID != "" {
		message.finalized[rowID] = struct{}{}
	}

	blocks, err := native.ContentBlocks()
	if err != nil {
		return err
	}

	responseID := native.ID
	if native.Model == nativeSyntheticModel {
		responseID = ""
	}

	for index := range blocks {
		block := &blocks[index]
		switch block.Type {
		case contentBlockTypeText, contentBlockTypeThinking:
			text, pending := block.Text, &message.text
			if block.Type == contentBlockTypeThinking {
				text, pending = block.Thinking, &message.thinking
			}

			text = consumeAssistantText(pending, text)
			if text == "" {
				continue
			}

			update := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text), MessageId: optionalString(responseID)}}
			if block.Type == contentBlockTypeThinking {
				update = acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(text), MessageId: optionalString(responseID)}}
			}

			if err := s.emit(ctx, update); err != nil {
				return err
			}
		case contentBlockTypeToolCall:
			if err := s.publishToolStart(ctx, state, *block); err != nil {
				return err
			}
		case contentBlockTypeImage:
			if link := remoteImageLink(*block); link != nil {
				key := native.ID + ":" + link.ResourceLink.Uri
				if _, emitted := message.images[key]; emitted {
					continue
				}

				if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: *link, MessageId: optionalString(responseID)}}); err != nil {
					return err
				}

				message.images[key] = struct{}{}

				continue
			}

			output, failure := decodeOutputImage(*block, s.agent.options.ImageLimits.core().EffectiveOutputPerImage())
			if failure != nil {
				if state.replay {
					return failure
				}

				guidance, _ := failure.Guidance()
				if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(guidance), MessageId: optionalString(responseID)}}); err != nil {
					return err
				}

				continue
			}

			key := native.ID + ":" + output.Fingerprint
			if _, emitted := message.images[key]; emitted {
				continue
			}

			if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ImageBlock(output.Data, output.MIME), MessageId: optionalString(responseID)}}); err != nil {
				return err
			}

			message.images[key] = struct{}{}
			state.imagesEmitted = true
		}
	}

	if state.replay || parent != "" || responseID == "" || responseID == state.call {
		return nil
	}

	// A call no message_start announced reports its request here. The
	// record's output_tokens is the stream's opening figure, so output is
	// left out.
	state.call, state.callOpen = responseID, false
	record := callUsage(native.Usage)
	record.ResponseID = responseID
	record.OutputTokens = nil

	return s.emitContext(ctx, state, requestTokens(record), record)
}

func (s *session) projectToolResults(ctx context.Context, state *cycleState, blocks []claude.ContentBlock) error {
	for index := range blocks {
		block := &blocks[index]
		if block.Type != "tool_result" {
			continue
		}

		content, err := claude.DecodeContent(block.Content)
		if err != nil {
			return err
		}

		status := acp.ToolCallStatusCompleted
		if block.IsError {
			status = acp.ToolCallStatusFailed
		}

		if err := s.publishToolTerminal(ctx, state, block.ToolUseID, status, content); err != nil {
			return err
		}
	}

	return nil
}

// consumeAssistantText matches one completed content fragment to pending deltas.
func consumeAssistantText(pending *string, full string) string {
	if full == "" {
		return ""
	}

	if after, ok := strings.CutPrefix(*pending, full); ok {
		*pending = after

		return ""
	}

	if !strings.HasPrefix(full, *pending) {
		return ""
	}

	suffix := strings.TrimPrefix(full, *pending)
	*pending = ""

	return suffix
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

// mapUsage is the turn's summed consumption as result reports it; a report
// that states nothing is unknown.
func mapUsage(usage claude.Usage) *acp.Usage {
	call := callUsage(usage)
	if !call.Known() {
		return nil
	}

	input, output := tokens(call.InputTokens), tokens(call.OutputTokens)

	return &acp.Usage{InputTokens: input, OutputTokens: output, CachedReadTokens: call.CachedReadTokens, CachedWriteTokens: call.CachedWriteTokens, TotalTokens: input + output + tokens(call.CachedReadTokens) + tokens(call.CachedWriteTokens)}
}

// callUsage maps a native usage report onto the call breakdown. Claude's
// input_tokens already excludes the cache tokens.
func callUsage(usage claude.Usage) wire.CallUsage {
	return wire.CallUsage{InputTokens: usage.InputTokens, CachedReadTokens: usage.CacheReadInputTokens, CachedWriteTokens: usage.CacheCreationInputTokens, OutputTokens: usage.OutputTokens}
}

// tokens is a reported figure, 0 when the report left it out.
func tokens(member *int) int {
	if member == nil {
		return 0
	}

	return *member
}

// requestTokens is the context a model call's request occupies, counted as
// claude counts it: input plus cache reads and cache writes.
func requestTokens(usage wire.CallUsage) int {
	return tokens(usage.InputTokens) + tokens(usage.CachedReadTokens) + tokens(usage.CachedWriteTokens)
}

// responseUsage is a finished model call's breakdown. Its request members come
// from message_delta where the provider restates the request, each falling
// back to the call's message_start when message_delta leaves it out; its
// output comes from message_delta alone, since message_start states only an
// opening figure.
func responseUsage(start wire.CallUsage, end wire.CallUsage) wire.CallUsage {
	call := wire.CallUsage{InputTokens: start.InputTokens, CachedReadTokens: start.CachedReadTokens, CachedWriteTokens: start.CachedWriteTokens, OutputTokens: end.OutputTokens}
	if requestTokens(end) > 0 {
		call.InputTokens = cmp.Or(end.InputTokens, start.InputTokens)
		call.CachedReadTokens = cmp.Or(end.CachedReadTokens, start.CachedReadTokens)
		call.CachedWriteTokens = cmp.Or(end.CachedWriteTokens, start.CachedWriteTokens)
	}

	return call
}

// emitResponseInput reports the context a top-level model call's request
// occupies the moment the call starts, before its thinking and output stream.
// A provider that reports usage only at the end of the stream opens the call
// with an empty report, which states nothing.
func (s *session) emitResponseInput(ctx context.Context, state *cycleState, message claude.Message) error {
	start := callUsage(message.Usage)
	if !start.Known() {
		start = wire.CallUsage{}
	}

	state.call, state.callStart, state.callOpen = message.ID, start, true
	if message.Model != "" {
		state.model = message.Model
	}

	return s.emitContext(ctx, state, requestTokens(start), wire.CallUsage{})
}

// emitResponseUsage reports the context a top-level model call leaves
// occupied once its message_delta states the whole output, with the call's
// breakdown. An empty report, as a gateway replaying a cached response sends,
// states nothing and leaves the last figure in place.
func (s *session) emitResponseUsage(ctx context.Context, state *cycleState, usage claude.Usage) error {
	if !state.callOpen {
		return nil
	}

	state.callOpen = false

	end := callUsage(usage)
	if !end.Known() {
		return nil
	}

	call := responseUsage(state.callStart, end)
	call.ResponseID = state.call

	request := requestTokens(call)
	if request == 0 {
		return nil
	}

	return s.emitContext(ctx, state, request+tokens(call.OutputTokens), call)
}

// emitContext reports one model call's context occupancy, with the call's
// breakdown when the update reports its response, and keeps it as the cycle's
// latest figure. A call without a figure reports nothing. size is the
// session's known context window.
func (s *session) emitContext(ctx context.Context, state *cycleState, used int, call wire.CallUsage) error {
	if used <= 0 {
		return nil
	}

	state.context = used

	return s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Size: s.knownContextWindow(), Used: used, Meta: call.Apply(nil)}})
}

// emitSettledUsage reports a settled cycle: the context its last model call
// left, the session's cumulative cost, and the structured result. Without a
// figure since the cycle opened or claude compacted, nothing is sent unless a
// structured result must still reach the host, which then reports used as 0.
func (s *session) emitSettledUsage(ctx context.Context, state *cycleState) {
	if state.context == 0 && state.structuredOutput == nil {
		return
	}

	update := &acp.SessionUsageUpdate{Size: s.knownContextWindow(), Used: state.context}
	if state.cost > 0 {
		update.Cost = &acp.Cost{Amount: state.cost, Currency: costCurrency}
	}

	if state.structuredOutput != nil {
		update.Meta = map[string]any{vendor: map[string]any{metaStructuredOutputKey: state.structuredOutput}}
	}

	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: update})
}

// knownContextWindow is the selected model's context window as claude last
// reported it, else 0.
func (s *session) knownContextWindow() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return int(s.contextWindow)
}

// refreshContextWindow adopts the selected model's context window as claude
// reports it; a failed read leaves it unknown.
func (s *session) refreshContextWindow(ctx context.Context, rt *runtime) {
	window, err := rt.client.ContextWindow(ctx)
	if err != nil {
		window = 0
	}

	s.mu.Lock()
	s.contextWindow = window
	s.mu.Unlock()
}

func (s *session) emitRestoredUsage(ctx context.Context, rt *runtime) {
	stats, err := rt.client.ContextUsage(ctx)
	if err != nil {
		return
	}

	if stats.MaxTokens > 0 {
		s.mu.Lock()
		s.contextWindow = stats.MaxTokens
		s.mu.Unlock()
	}

	// An empty estimate states nothing.
	if stats.TotalTokens <= 0 {
		return
	}

	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Size: s.knownContextWindow(), Used: int(stats.TotalTokens)}})
}

func suppressedCommand(name string) bool {
	switch name {
	case "clear", "reset", "new", configCommand, "settings", "logout", "login", "mcp", "resume", "fork":
		return true
	}

	return false
}

// emitSessionInfo records the turn's time and, on the first prompt, a title.
func (s *session) emitSessionInfo(ctx context.Context, prompt []acp.ContentBlock) {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	update := acp.SessionSessionInfoUpdate{UpdatedAt: &updatedAt}

	s.mu.Lock()
	s.updatedAt = updatedAt

	if s.title == "" {
		if title := wire.PromptTitle(prompt); title != "" {
			s.title = title
			update.Title = &title
		}
	}
	s.mu.Unlock()

	_ = s.emit(ctx, acp.SessionUpdate{SessionInfoUpdate: &update})
}

func (s *session) sessionInfo() acp.SessionInfo {
	s.mu.Lock()
	title := s.title
	updatedAt := s.updatedAt
	s.mu.Unlock()

	if title == "" {
		title = string(s.id)
	}

	info := acp.SessionInfo{
		Meta:                  wire.NativeSessionMeta(vendor, s.nativeID),
		SessionId:             s.id,
		Title:                 &title,
		Cwd:                   s.cwd,
		AdditionalDirectories: append([]string(nil), s.additionalDirectories...),
	}
	if updatedAt != "" {
		info.UpdatedAt = &updatedAt
	}

	return info
}

// publishCommands emits the session's command catalog as a full replacement,
// including the explicit empty one.
func (s *session) publishCommands(ctx context.Context) error {
	s.mu.Lock()
	commands := append([]acp.AvailableCommand{}, s.commands...)
	s.mu.Unlock()

	return s.emit(ctx, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{AvailableCommands: commands}})
}

func (s *session) clearCommands(ctx context.Context) {
	s.openMu.Lock()
	defer s.openMu.Unlock()

	s.mu.Lock()
	s.commands = nil
	closing := s.closing
	s.mu.Unlock()

	if !closing {
		_ = s.publishCommands(ctx)
	}
}

// availableCommands converts claude's command catalog, dropping names the shared
// sanitizer rejects.
func availableCommands(commands []claude.Command) []acp.AvailableCommand {
	available := make([]acp.AvailableCommand, 0, len(commands))

	for _, command := range commands {
		if !wire.ValidCommandName(command.Name) || suppressedCommand(command.Name) {
			continue
		}

		available = append(available, acp.AvailableCommand{Name: command.Name, Description: command.Description})
	}

	return available
}

// emitRawEvent forwards one native record on the raw-event channel when the
// session opted in. Inline payloads and source URLs are redacted.
func (s *session) emitRawEvent(ctx context.Context, event claude.Event) {
	if !s.rawEvents.Enabled() {
		return
	}

	conn := s.agent.connection()
	if conn == nil {
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(event.Raw, &payload); err != nil {
		return
	}

	redactImages(payload)

	notify := func(ctx context.Context, method string, params map[string]any) error {
		return conn.NotifyExtension(ctx, method, params)
	}

	if err := s.rawEvents.Emit(ctx, notify, payload); err != nil {
		s.agent.observe.RecordRawMessageEmitFailure(ctx, err)
	}
}

func redactImages(value any) {
	switch typed := value.(type) {
	case map[string]any:
		blockType, _ := typed["type"].(string)
		if blockType == nativeSourceURL {
			if _, exists := typed["url"]; exists {
				typed["url"] = "[redacted]"
			}
		}

		if data, ok := typed["data"].(string); ok && blockType == nativeBase64 && data != "" {
			typed["data"] = ""
			typed["sizeBytes"] = len(data) / 4 * 3
		}

		for _, item := range typed {
			redactImages(item)
		}
	case []any:
		for _, item := range typed {
			redactImages(item)
		}
	}
}

// Native record, stream, and settings literals.
const (
	configCommand        = "config"
	mimePDF              = "application/pdf"
	nativeBase64         = "base64"
	nativeControlRequest = "control_request"
	nativeDefault        = "default"
	nativeEffortLevel    = "effortLevel"
	nativeMessageStart   = "message_start"
	nativeMessageDelta   = "message_delta"
	// nativeCompactBoundary is the system record claude emits once it has
	// replaced its context with a summary.
	nativeCompactBoundary = "compact_boundary"
	nativeOutputStyle     = "outputStyle"
	nativeResult          = "result"
	nativeStreamEvent     = "stream_event"
	// nativeSyntheticModel is the model of an assistant record claude writes
	// itself, such as an API error notice; its message id is claude's own
	// uuid rather than a model response's id.
	nativeSyntheticModel = "<synthetic>"
	nativeSystem         = "system"
	// nativeSourceURL is the source type of a native image the harness serves
	// by link rather than inline.
	nativeSourceURL    = "url"
	permissionModePlan = "plan"
)
