package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	MessageTypeUser      = "user"
	MessageTypeAssistant = "assistant"
	MessageTypeResult    = "result"
	MessageTypeStream    = "stream_event"
	MessageTypeSystem    = "system"
	MessageTypeMirror    = "transcript_mirror"

	BlockTypeText         = "text"
	BlockTypeThinking     = "thinking"
	BlockTypeToolUse      = "tool_use"
	BlockTypeToolResult   = "tool_result"
	BlockTypeServerUse    = "server_tool_use"
	BlockTypeServerResult = "advisor_tool_result"
)

const rawJSONInternalKey = "\x00raw_json"

// Message is a parsed Claude stream-json message.
type Message interface {
	ClaudeType() string
	RawMessage() map[string]any
	RawJSON() string
}

// TextBlock contains text content.
type TextBlock struct {
	Text string
}

// ThinkingBlock contains Claude thinking text.
type ThinkingBlock struct {
	Thinking  string
	Signature string
}

// ToolUseBlock contains a client-side tool call.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResultBlock contains a client-side tool result.
type ToolResultBlock struct {
	ToolUseID string
	Content   []ContentBlock
	IsError   bool
	Raw       map[string]any
}

// UnknownBlock preserves unrecognized content.
type UnknownBlock struct {
	Type string
	Raw  map[string]any
}

// ContentBlock is a Claude content block.
type ContentBlock interface {
	BlockType() string
}

func (b TextBlock) BlockType() string       { return BlockTypeText }
func (b ThinkingBlock) BlockType() string   { return BlockTypeThinking }
func (b ToolUseBlock) BlockType() string    { return BlockTypeToolUse }
func (b ToolResultBlock) BlockType() string { return BlockTypeToolResult }
func (b UnknownBlock) BlockType() string    { return b.Type }

// UserMessage is a parsed user message.
type UserMessage struct {
	Content         any
	ParentToolUseID string
	Raw             map[string]any
	RawJSONText     string
}

func (m *UserMessage) ClaudeType() string         { return MessageTypeUser }
func (m *UserMessage) RawMessage() map[string]any { return m.Raw }
func (m *UserMessage) RawJSON() string            { return m.RawJSONText }

// AssistantMessage is a parsed assistant message.
type AssistantMessage struct {
	Content         []ContentBlock
	MessageID       string
	Model           string
	ParentToolUseID string
	StopReason      string
	ErrorKind       string
	Raw             map[string]any
	RawJSONText     string
}

func (m *AssistantMessage) ClaudeType() string         { return MessageTypeAssistant }
func (m *AssistantMessage) RawMessage() map[string]any { return m.Raw }
func (m *AssistantMessage) RawJSON() string            { return m.RawJSONText }

// ResultMessage is the terminal result for a Claude turn.
type ResultMessage struct {
	Subtype          string
	IsError          bool
	Origin           map[string]any
	StopReason       string
	Result           string
	Error            string
	StructuredOutput map[string]any
	TotalCostUSD     *float64
	Usage            *Usage
	ModelUsage       map[string]ModelUsage
	Errors           []string
	Raw              map[string]any
	RawJSONText      string
}

func (m *ResultMessage) ClaudeType() string         { return MessageTypeResult }
func (m *ResultMessage) RawMessage() map[string]any { return m.Raw }
func (m *ResultMessage) RawJSON() string            { return m.RawJSONText }

// Usage contains token usage from Claude result messages.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CachedInputTokens        int
	CacheCreationInputTokens int
	ReasoningOutputTokens    int
}

// ModelUsage contains per-model token and context usage from Claude result messages.
type ModelUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	ContextWindow            int
}

// SystemMessage is a parsed system message.
type SystemMessage struct {
	Subtype     string
	Raw         map[string]any
	RawJSONText string
}

func (m *SystemMessage) ClaudeType() string         { return MessageTypeSystem }
func (m *SystemMessage) RawMessage() map[string]any { return m.Raw }
func (m *SystemMessage) RawJSON() string            { return m.RawJSONText }

// StreamEventMessage is a partial Claude stream event.
type StreamEventMessage struct {
	EventType       string
	Event           map[string]any
	ParentToolUseID string
	Raw             map[string]any
	RawJSONText     string
}

func (m *StreamEventMessage) ClaudeType() string         { return MessageTypeStream }
func (m *StreamEventMessage) RawMessage() map[string]any { return m.Raw }
func (m *StreamEventMessage) RawJSON() string            { return m.RawJSONText }

// TranscriptMirrorMessage carries Claude's native transcript mirror frame.
type TranscriptMirrorMessage struct {
	FilePath    string
	Entries     []json.RawMessage
	Raw         map[string]any
	RawJSONText string
}

func (m *TranscriptMirrorMessage) ClaudeType() string         { return MessageTypeMirror }
func (m *TranscriptMirrorMessage) RawMessage() map[string]any { return m.Raw }
func (m *TranscriptMirrorMessage) RawJSON() string            { return m.RawJSONText }

// UnknownMessage preserves a message the parser does not understand.
type UnknownMessage struct {
	Type        string
	Raw         map[string]any
	RawJSONText string
}

func (m *UnknownMessage) ClaudeType() string         { return m.Type }
func (m *UnknownMessage) RawMessage() map[string]any { return m.Raw }
func (m *UnknownMessage) RawJSON() string            { return m.RawJSONText }

// ParseMessage parses one Claude stream-json map.
func ParseMessage(raw map[string]any) (Message, error) {
	raw, rawJSON := splitRawJSON(raw)

	msgType := stringValue(raw[keyType])
	switch msgType {
	case MessageTypeUser:
		return parseUser(raw, rawJSON), nil
	case MessageTypeAssistant:
		return parseAssistant(raw, rawJSON)
	case MessageTypeResult:
		return parseResult(raw, rawJSON), nil
	case MessageTypeStream:
		return parseStreamEvent(raw, rawJSON), nil
	case MessageTypeSystem:
		return parseSystem(raw, rawJSON), nil
	case MessageTypeMirror:
		return parseTranscriptMirror(raw, rawJSON)
	default:
		return &UnknownMessage{Type: msgType, Raw: raw, RawJSONText: rawJSON}, nil
	}
}

func splitRawJSON(raw map[string]any) (map[string]any, string) {
	rawJSON := stringValue(raw[rawJSONInternalKey])
	if rawJSON == "" {
		return raw, ""
	}

	clean := make(map[string]any, len(raw)-1)
	for key, value := range raw {
		if key == rawJSONInternalKey {
			continue
		}

		clean[key] = value
	}

	return clean, rawJSON
}

func parseUser(raw map[string]any, rawJSON string) *UserMessage {
	return &UserMessage{
		Content:         raw[keyContent],
		ParentToolUseID: stringValue(raw[keyParentToolID]),
		Raw:             raw,
		RawJSONText:     rawJSON,
	}
}

func parseAssistant(raw map[string]any, rawJSON string) (*AssistantMessage, error) {
	messageData, _ := raw[keyMessage].(map[string]any)
	if messageData == nil {
		return nil, fmt.Errorf("assistant message missing message object")
	}

	content, err := parseBlocks(messageData[keyContent])
	if err != nil {
		return nil, err
	}

	return &AssistantMessage{
		Content:         content,
		MessageID:       stringValue(raw["uuid"]),
		Model:           stringValue(messageData[keyModel]),
		ParentToolUseID: stringValue(raw[keyParentToolID]),
		StopReason:      stringValue(messageData[keyStopReason]),
		ErrorKind:       stringValue(raw[keyError]),
		Raw:             raw,
		RawJSONText:     rawJSON,
	}, nil
}

func parseResult(raw map[string]any, rawJSON string) *ResultMessage {
	return &ResultMessage{
		Subtype:          stringValue(raw[keySubtype]),
		IsError:          boolValue(raw[keyIsError]),
		Origin:           mapValue(raw[keyOrigin]),
		StopReason:       stringValue(raw[keyStopReason]),
		Result:           stringValue(raw[keyResult]),
		Error:            stringValue(raw[keyError]),
		StructuredOutput: mapValue(raw[keyStructuredOutput]),
		TotalCostUSD:     floatPtr(raw[keyTotalCostUSD]),
		Usage:            parseUsage(raw["usage"]),
		ModelUsage:       parseModelUsage(raw["modelUsage"]),
		Errors:           stringSlice(raw[keyErrors]),
		Raw:              raw,
		RawJSONText:      rawJSON,
	}
}

func parseSystem(raw map[string]any, rawJSON string) *SystemMessage {
	return &SystemMessage{Subtype: stringValue(raw[keySubtype]), Raw: raw, RawJSONText: rawJSON}
}

func parseStreamEvent(raw map[string]any, rawJSON string) *StreamEventMessage {
	event := mapValue(raw[keyEvent])

	return &StreamEventMessage{
		EventType:       stringValue(event[keyType]),
		Event:           event,
		ParentToolUseID: stringValue(raw[keyParentToolID]),
		Raw:             raw,
		RawJSONText:     rawJSON,
	}
}

func parseTranscriptMirror(raw map[string]any, rawJSON string) (*TranscriptMirrorMessage, error) {
	frame := &TranscriptMirrorMessage{
		FilePath:    stringValue(raw["filePath"]),
		Raw:         raw,
		RawJSONText: rawJSON,
	}

	if rawJSON != "" {
		var decoded struct {
			Entries []json.RawMessage `json:"entries"`
		}
		if err := json.Unmarshal([]byte(rawJSON), &decoded); err != nil {
			return nil, fmt.Errorf("decode transcript mirror raw json: %w", err)
		}

		frame.Entries = cleanMirrorEntries(decoded.Entries)

		return frame, nil
	}

	values, _ := raw["entries"].([]any)
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshal transcript mirror entry: %w", err)
		}

		frame.Entries = append(frame.Entries, data)
	}

	frame.Entries = cleanMirrorEntries(frame.Entries)

	return frame, nil
}

func cleanMirrorEntries(entries []json.RawMessage) []json.RawMessage {
	if len(entries) == 0 {
		return nil
	}

	clean := entries[:0]
	for _, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 {
			continue
		}

		clean = append(clean, append(json.RawMessage(nil), trimmed...))
	}

	if len(clean) == 0 {
		return nil
	}

	return clean
}

// ParseContentBlock parses one Claude content block.
func ParseContentBlock(raw map[string]any) ContentBlock {
	block, err := parseBlock(raw)
	if err != nil {
		return UnknownBlock{Type: stringValue(raw[keyType]), Raw: raw}
	}

	return block
}

func parseBlocks(raw any) ([]ContentBlock, error) {
	if text, ok := raw.(string); ok {
		if text == "" {
			return nil, nil
		}

		return []ContentBlock{TextBlock{Text: text}}, nil
	}

	values, _ := raw.([]any)
	blocks := make([]ContentBlock, 0, len(values))

	for _, value := range values {
		blockMap, _ := value.(map[string]any)
		if blockMap == nil {
			continue
		}

		block, err := parseBlock(blockMap)
		if err != nil {
			return nil, err
		}

		blocks = append(blocks, block)
	}

	return blocks, nil
}

func parseBlock(raw map[string]any) (ContentBlock, error) {
	blockType, ok := raw[keyType].(string)
	if !ok || blockType == "" {
		return nil, fmt.Errorf("content block missing string type")
	}

	switch blockType {
	case BlockTypeText:
		return TextBlock{Text: stringValue(raw[keyText])}, nil
	case BlockTypeThinking:
		return ThinkingBlock{
			Thinking:  stringValue(raw[keyThinking]),
			Signature: stringValue(raw[keySignature]),
		}, nil
	case BlockTypeToolUse, BlockTypeServerUse:
		id := stringValue(raw[keyID])
		if id == "" {
			return nil, fmt.Errorf("%s block missing id", blockType)
		}

		input, _ := raw[keyInput].(map[string]any)

		return ToolUseBlock{ID: id, Name: stringValue(raw[keyName]), Input: input}, nil
	case BlockTypeToolResult, BlockTypeServerResult:
		toolUseID := stringValue(raw[keyToolUseID])
		if toolUseID == "" {
			return nil, fmt.Errorf("%s block missing tool_use_id", blockType)
		}

		content, err := parseBlocks(raw[keyContent])
		if err != nil {
			return nil, err
		}

		return ToolResultBlock{
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   boolValue(raw[keyIsError]),
			Raw:       raw,
		}, nil
	default:
		return UnknownBlock{Type: blockType, Raw: raw}, nil
	}
}

func stringValue(value any) string {
	typed, _ := value.(string)

	return typed
}

func boolValue(value any) bool {
	typed, _ := value.(bool)

	return typed
}

func mapValue(value any) map[string]any {
	typed, _ := value.(map[string]any)

	return typed
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}

		return 0
	default:
		return 0
	}
}

func floatPtr(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case float32:
		converted := float64(typed)

		return &converted
	case int:
		converted := float64(typed)

		return &converted
	case int64:
		converted := float64(typed)

		return &converted
	case string:
		converted, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err == nil {
			return &converted
		}

		return nil
	default:
		return nil
	}
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}

		return values
	default:
		return nil
	}
}

func parseUsage(value any) *Usage {
	raw, _ := value.(map[string]any)
	if raw == nil {
		return nil
	}

	return &Usage{
		InputTokens:              intValue(raw["input_tokens"]),
		OutputTokens:             intValue(raw["output_tokens"]),
		CachedInputTokens:        intValue(raw["cache_read_input_tokens"]),
		CacheCreationInputTokens: intValue(raw["cache_creation_input_tokens"]),
		ReasoningOutputTokens:    intValue(raw["reasoning_output_tokens"]),
	}
}

func parseModelUsage(value any) map[string]ModelUsage {
	raw, _ := value.(map[string]any)
	if raw == nil {
		return nil
	}

	usage := make(map[string]ModelUsage, len(raw))
	for model, value := range raw {
		modelRaw, _ := value.(map[string]any)
		if modelRaw == nil {
			continue
		}

		usage[model] = ModelUsage{
			InputTokens:              intValue(modelRaw["inputTokens"]),
			OutputTokens:             intValue(modelRaw["outputTokens"]),
			CacheReadInputTokens:     intValue(modelRaw["cacheReadInputTokens"]),
			CacheCreationInputTokens: intValue(modelRaw["cacheCreationInputTokens"]),
			ContextWindow:            intValue(modelRaw["contextWindow"]),
		}
	}

	if len(usage) == 0 {
		return nil
	}

	return usage
}
