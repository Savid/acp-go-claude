package claudeacp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
)

type agentClient interface {
	Done() <-chan struct{}
	UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error
	CreateElicitation(context.Context, acp.UnstableCreateElicitationRequest, elicitationScope) (acp.UnstableCreateElicitationResponse, error)
	UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
	ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error)
	WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error)
	UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error)
	UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error)
	UnstableMessageMcp(context.Context, acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error)
	UnstableNotifyMcp(context.Context, acp.UnstableMessageMcpNotification) error
	RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	SessionUpdate(context.Context, acp.SessionNotification) error
	CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error)
	KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error)
	TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error)
	ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error)
	WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error)
	NotifyExtension(context.Context, string, any) error
}

type elicitationScope struct {
	SessionID  acp.SessionId
	ToolCallID acp.ToolCallId
	RequestID  *acp.RequestId
}

func scopedElicitationParams(
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (json.RawMessage, error) {
	var payload map[string]any

	switch {
	case params.Form != nil:
		payload = map[string]any{
			jsonFieldMessage:  params.Form.Message,
			jsonFieldMode:     claude.ElicitationModeForm,
			"requestedSchema": params.Form.RequestedSchema,
		}
		if len(params.Form.Meta) > 0 {
			payload[jsonFieldMeta] = params.Form.Meta
		}
	case params.Url != nil:
		payload = map[string]any{
			"elicitationId":  params.Url.ElicitationId,
			jsonFieldMessage: params.Url.Message,
			jsonFieldMode:    claude.ElicitationModeURL,
			jsonFieldURL:     params.Url.Url,
		}
		if len(params.Url.Meta) > 0 {
			payload[jsonFieldMeta] = params.Url.Meta
		}
	default:
		return nil, errors.New("elicitation request must include form or url")
	}

	if scope.SessionID != "" {
		payload[acpFieldSessionID] = scope.SessionID
	}

	if scope.ToolCallID != "" {
		payload["toolCallId"] = scope.ToolCallID
	}

	if scope.RequestID != nil {
		payload["requestId"] = scope.RequestID
	}

	return json.Marshal(payload)
}

func requestError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}

	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	if errors.Is(err, context.Canceled) {
		return acp.NewRequestCancelled(map[string]any{jsonFieldError: err.Error()})
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: err.Error()})
}
