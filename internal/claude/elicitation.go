package claude

import "context"

const (
	ElicitationModeForm = "form"
	ElicitationModeURL  = "url"

	ElicitationActionAccept  = "accept"
	ElicitationActionDecline = "decline"
	ElicitationActionCancel  = "cancel"
)

// ElicitationHandler handles Claude MCP elicitation control requests.
type ElicitationHandler func(ctx context.Context, request ElicitationRequest) (ElicitationResponse, error)

// ElicitationRequest describes a Claude MCP elicitation request.
type ElicitationRequest struct {
	MCPServerName   string
	Message         string
	Mode            string
	URL             string
	ElicitationID   string
	ToolUseID       string
	RequestedSchema map[string]any
	Raw             map[string]any
}

// ElicitationResponse is serialized back to Claude's elicitation response.
type ElicitationResponse struct {
	Action  string
	Content map[string]any
}

func (r ElicitationRequest) requestedMode() string {
	if r.Mode != "" {
		return r.Mode
	}

	if r.URL != "" {
		return ElicitationModeURL
	}

	return ElicitationModeForm
}

func (r ElicitationResponse) toPayload() map[string]any {
	action := r.Action
	if action == "" {
		action = ElicitationActionDecline
	}

	payload := map[string]any{"action": action}
	if r.Content != nil {
		payload["content"] = r.Content
	}

	return payload
}
