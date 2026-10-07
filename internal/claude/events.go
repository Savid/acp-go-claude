package claude

import "encoding/json"

type CompactMetadata struct {
	PostTokens *int   `json:"post_tokens"` //nolint:tagliatelle // Native token fields use snake_case.
	Trigger    string `json:"trigger"`
	PreTokens  *int   `json:"pre_tokens"` //nolint:tagliatelle // Native token fields use snake_case.
}

type Event struct {
	CompactMetadata  *CompactMetadata      `json:"compact_metadata"` //nolint:tagliatelle // Native compaction metadata uses snake_case.
	Model            string                `json:"model"`
	Type             string                `json:"type"`
	Subtype          string                `json:"subtype"`
	UUID             string                `json:"uuid"`
	SessionID        string                `json:"session_id"`         //nolint:tagliatelle // Claude uses this native wire spelling.
	ParentToolUseID  string                `json:"parent_tool_use_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Message          *Message              `json:"message"`
	Event            *StreamEvent          `json:"event"`
	RequestID        string                `json:"request_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Request          *ControlRequest       `json:"request"`
	IsError          bool                  `json:"is_error"`    //nolint:tagliatelle // Claude uses this native wire spelling.
	StopReason       string                `json:"stop_reason"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Result           string                `json:"result"`
	Error            string                `json:"error"`
	Errors           []string              `json:"errors"`
	StructuredOutput any                   `json:"structured_output"` //nolint:tagliatelle // Claude uses this native wire spelling.
	TotalCostUSD     float64               `json:"total_cost_usd"`    //nolint:tagliatelle // Claude uses this native wire spelling.
	Usage            Usage                 `json:"usage"`
	ModelUsage       map[string]ModelUsage `json:"modelUsage"`
	Raw              json.RawMessage       `json:"-"`
}

// StreamEvent is one partial-message event of the model call in flight.
// message_start carries the call's request usage; message_delta carries its
// final usage, whose output_tokens is the call's whole output.
type StreamEvent struct {
	Type    string   `json:"type"`
	Message *Message `json:"message"`
	Usage   Usage    `json:"usage"`
	Delta   struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

// Message is one native message. An assistant record's usage repeats its
// call's message_start usage, so its output_tokens is not the call's output.
type Message struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Usage      Usage           `json:"usage"`
}

func (m Message) ContentBlocks() ([]ContentBlock, error) { return DecodeContent(m.Content) }
func DecodeContent(raw json.RawMessage) ([]ContentBlock, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []ContentBlock{{Type: "text", Text: text}}, nil
	}

	var blocks []ContentBlock

	err := json.Unmarshal(raw, &blocks)

	return blocks, err
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Source    *Source         `json:"source,omitempty"`
}

type Source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Usage is one native usage report. A nil member is a figure the report left
// out or sent as null. InputTokens excludes the cache tokens.
type Usage struct {
	InputTokens              *int `json:"input_tokens"`                //nolint:tagliatelle // Claude uses this native wire spelling.
	OutputTokens             *int `json:"output_tokens"`               //nolint:tagliatelle // Claude uses this native wire spelling.
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"` //nolint:tagliatelle // Claude uses this native wire spelling.
}

type ModelUsage struct {
	ContextWindow int64 `json:"contextWindow"`
}

// ContextUsage is the get_context_usage answer: claude's estimate of the
// tokens in context and the selected model's context window.
type ContextUsage struct {
	TotalTokens int64 `json:"totalTokens"`
	MaxTokens   int64 `json:"maxTokens"`
}

type Model struct {
	Value                 string   `json:"value"`
	DisplayName           string   `json:"displayName"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
	SupportsAutoMode      bool     `json:"supportsAutoMode"`
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type InitializeResponse struct {
	Models                []Model   `json:"models"`
	Commands              []Command `json:"commands"`
	OutputStyle           string    `json:"output_style"`            //nolint:tagliatelle // Claude uses this native wire spelling.
	AvailableOutputStyles []string  `json:"available_output_styles"` //nolint:tagliatelle // Claude uses this native wire spelling.
	PermissionMode        string    `json:"current_permission_mode"` //nolint:tagliatelle // Claude uses this native wire spelling.
}

type ControlRequest struct {
	Subtype         string          `json:"subtype"`
	ToolName        string          `json:"tool_name"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Input           map[string]any  `json:"input"`
	ToolUseID       string          `json:"tool_use_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	Mode            string          `json:"mode"`
	URL             string          `json:"url"`
	ElicitationID   string          `json:"elicitationId"`
}
