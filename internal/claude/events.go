package claude

import "encoding/json"

type Event struct {
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

type StreamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *Message      `json:"message"`
	ContentBlock *ContentBlock `json:"content_block"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"` //nolint:tagliatelle // Claude uses this native wire spelling.
		StopReason  string `json:"stop_reason"`  //nolint:tagliatelle // Claude uses this native wire spelling.
	} `json:"delta"`
}

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
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`                //nolint:tagliatelle // Claude uses this native wire spelling.
	OutputTokens             int64 `json:"output_tokens"`               //nolint:tagliatelle // Claude uses this native wire spelling.
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"` //nolint:tagliatelle // Claude uses this native wire spelling.
}
type ModelUsage struct {
	ContextWindow int64 `json:"contextWindow"`
}
type ContextUsage struct {
	TotalTokens int64  `json:"totalTokens"`
	MaxTokens   int64  `json:"maxTokens"`
	Model       string `json:"model"`
}
type Model struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
	SupportsAutoMode      bool     `json:"supportsAutoMode"`
}
type Command struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argumentHint"`
}
type InitializeResponse struct {
	Models                []Model   `json:"models"`
	Commands              []Command `json:"commands"`
	OutputStyle           string    `json:"output_style"`            //nolint:tagliatelle // Claude uses this native wire spelling.
	AvailableOutputStyles []string  `json:"available_output_styles"` //nolint:tagliatelle // Claude uses this native wire spelling.
	PermissionMode        string    `json:"current_permission_mode"` //nolint:tagliatelle // Claude uses this native wire spelling.
}
type ControlRequest struct {
	Subtype               string           `json:"subtype"`
	ToolName              string           `json:"tool_name"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Input                 map[string]any   `json:"input"`
	ToolUseID             string           `json:"tool_use_id"`            //nolint:tagliatelle // Claude uses this native wire spelling.
	PermissionSuggestions []map[string]any `json:"permission_suggestions"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Title                 string           `json:"title"`
	Message               string           `json:"message"`
	RequestedSchema       json.RawMessage  `json:"requestedSchema"`
	Mode                  string           `json:"mode"`
	URL                   string           `json:"url"`
	ElicitationID         string           `json:"elicitationId"`
	CallbackID            string           `json:"callback_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
}
