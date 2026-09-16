package claudeacp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

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
	finalized          map[string]struct{}
	images             map[string]struct{}
}
type cycleState struct {
	replay           bool
	usage            *acp.Usage
	cost             float64
	structuredOutput any
	stopReason       string
	errorMessage     string
	imagesEmitted    bool
	messages         map[string]*messageState
	tools            map[string]*toolState
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
			case update.UsageUpdate != nil:
				update.UsageUpdate.Meta = meta
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

		for _, usage := range event.ModelUsage {
			if usage.ContextWindow > 0 {
				s.mu.Lock()
				s.contextWindow = usage.ContextWindow
				s.mu.Unlock()

				break
			}
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

	switch native.Type {
	case nativeMessageStart:
		message.text = ""

		message.thinking = ""
		if native.Message != nil {
			message.id = native.Message.ID
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

			update := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text), MessageId: optionalString(native.ID)}}
			if block.Type == contentBlockTypeThinking {
				update = acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(text), MessageId: optionalString(native.ID)}}
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

				if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: *link, MessageId: optionalString(native.ID)}}); err != nil {
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
				if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(guidance), MessageId: optionalString(native.ID)}}); err != nil {
					return err
				}

				continue
			}

			key := native.ID + ":" + output.Fingerprint
			if _, emitted := message.images[key]; emitted {
				continue
			}

			if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ImageBlock(output.Data, output.MIME), MessageId: optionalString(native.ID)}}); err != nil {
				return err
			}

			message.images[key] = struct{}{}
			state.imagesEmitted = true
		}
	}

	return nil
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
func mapUsage(usage claude.Usage) *acp.Usage {
	read, write := int(usage.CacheReadInputTokens), int(usage.CacheCreationInputTokens)

	return &acp.Usage{InputTokens: int(usage.InputTokens), OutputTokens: int(usage.OutputTokens), CachedReadTokens: &read, CachedWriteTokens: &write, TotalTokens: int(usage.InputTokens+usage.OutputTokens) + read + write}
}
func (s *session) emitUsage(ctx context.Context, state *cycleState, stats *claude.ContextUsage) {
	s.mu.Lock()
	size := s.contextWindow
	s.mu.Unlock()

	used := 0
	if state.usage != nil {
		used = state.usage.TotalTokens
	}

	if stats != nil {
		used = int(stats.TotalTokens)

		// A native reading that cannot state the window keeps the one the
		// turn's model usage already proved.
		if stats.MaxTokens > 0 {
			size = stats.MaxTokens
		}
	}

	update := &acp.SessionUsageUpdate{Size: int(size), Used: used}
	if state.cost > 0 {
		update.Cost = &acp.Cost{Amount: state.cost, Currency: "USD"}
	}

	if state.structuredOutput != nil {
		update.Meta = map[string]any{vendor: map[string]any{metaStructuredOutputKey: state.structuredOutput}}
	}

	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: update})
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

	s.emitUsage(ctx, &cycleState{}, &stats)
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
	s.mu.Lock()
	s.commands = nil
	s.mu.Unlock()

	_ = s.publishCommands(ctx)
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
	nativeOutputStyle    = "outputStyle"
	nativeResult         = "result"
	nativeStreamEvent    = "stream_event"
	nativeSystem         = "system"
	// nativeSourceURL is the source type of a native image the harness serves
	// by link rather than inline.
	nativeSourceURL    = "url"
	permissionModePlan = "plan"
)
