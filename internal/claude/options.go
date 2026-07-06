package claude

import (
	"context"
	"time"
)

// Options configures one Claude CLI session.
type Options struct {
	CLIPath string
	Cwd     string

	ClaudeHome string
	Env        map[string]string

	SessionID   string
	ResumeID    string
	ForkSession bool
	Bare        bool
	Model       string
	SystemText  string
	JSONSchema  map[string]any

	PermissionMode          string
	PermissionPromptTool    string
	AllowSkipPermissionsArg bool

	AddDirs []string

	MCPConfigJSON  string
	SettingSources []string
	// SettingsFile is an absolute path passed as --settings, loading an
	// additional Claude settings layer on top of the base settings.json.
	SettingsFile string

	InitializeTimeout     time.Duration
	ControlHandlerTimeout time.Duration

	PermissionHandler  PermissionHandler
	ElicitationHandler ElicitationHandler
	HookHandler        HookHandler

	SessionMirror bool
	Hooks         Hooks
}

const HookEventPostToolUse = "PostToolUse"

// Hooks configures Claude control-protocol hooks by event name.
type Hooks map[string][]HookMatcher

// HookMatcher describes one Claude hook matcher and its callback IDs.
type HookMatcher struct {
	Matcher         string
	HookCallbackIDs []string
	TimeoutSeconds  int
}

func (h Hooks) toPayload() map[string]any {
	payload := map[string]any{}

	for eventName, matchers := range h {
		if eventName == "" {
			continue
		}

		values := make([]map[string]any, 0, len(matchers))
		for _, matcher := range matchers {
			if len(matcher.HookCallbackIDs) == 0 {
				continue
			}

			value := map[string]any{
				keyHookCallback: append([]string(nil), matcher.HookCallbackIDs...),
			}
			if matcher.Matcher != "" {
				value["matcher"] = matcher.Matcher
			}

			if matcher.TimeoutSeconds > 0 {
				value["timeout"] = matcher.TimeoutSeconds
			}

			values = append(values, value)
		}

		if len(values) > 0 {
			payload[eventName] = values
		}
	}

	return payload
}

// HookHandler handles Claude hook_callback control requests.
type HookHandler func(ctx context.Context, request HookRequest) (HookResponse, error)

// HookRequest describes a Claude hook callback request.
type HookRequest struct {
	EventName    string
	ToolName     string
	ToolUseID    string
	ToolResponse map[string]any
}

// HookResponse is serialized back to Claude's hook_callback response.
type HookResponse struct {
	Continue bool
}

func (r HookResponse) toPayload() map[string]any {
	return map[string]any{"continue": r.Continue}
}

// PermissionHandler handles Claude can_use_tool control requests.
type PermissionHandler func(ctx context.Context, request PermissionRequest) (PermissionDecision, error)

// PermissionRequest describes a Claude permission request.
type PermissionRequest struct {
	ToolName    string
	ToolUseID   string
	Input       map[string]any
	Title       string
	Suggestions []map[string]any
	Raw         map[string]any
}

// PermissionDecision is serialized back to Claude's can_use_tool response.
type PermissionDecision struct {
	Behavior           string
	UpdatedInput       map[string]any
	Message            string
	Interrupt          bool
	UpdatedPermissions []map[string]any
}

func (d PermissionDecision) toPayload(originalInput map[string]any) map[string]any {
	behavior := d.Behavior
	if behavior == "" {
		behavior = BehaviorDeny
	}

	payload := map[string]any{keyBehavior: behavior}
	if behavior == BehaviorAllow {
		updated := d.UpdatedInput
		if updated == nil {
			updated = originalInput
		}

		if updated == nil {
			updated = map[string]any{}
		}

		payload["updatedInput"] = updated
	}

	if d.Message != "" {
		payload["message"] = d.Message
	}

	if d.Interrupt {
		payload["interrupt"] = true
	}

	if len(d.UpdatedPermissions) > 0 {
		payload["updatedPermissions"] = d.UpdatedPermissions
	}

	return payload
}
