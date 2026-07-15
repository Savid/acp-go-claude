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
	TurnNonce  string
	ToolCallID acp.ToolCallId
	RequestID  *string
}

func scopedElicitationParams(
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (json.RawMessage, error) {
	var (
		payload map[string]any
		meta    map[string]any
	)

	switch {
	case params.Form != nil:
		payload = map[string]any{
			jsonFieldMessage:  params.Form.Message,
			jsonFieldMode:     claude.ElicitationModeForm,
			"requestedSchema": params.Form.RequestedSchema,
		}
		meta = params.Form.Meta
	case params.Url != nil:
		payload = map[string]any{
			"elicitationId":  params.Url.ElicitationId,
			jsonFieldMessage: params.Url.Message,
			jsonFieldMode:    claude.ElicitationModeURL,
			jsonFieldURL:     params.Url.Url,
		}
		meta = params.Url.Meta
	default:
		return nil, errors.New("elicitation request must include form or url")
	}

	stamped, err := stampRouteMeta(meta, scope)
	if err != nil {
		return nil, err
	}

	payload[jsonFieldMeta] = stamped

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

func lifecycleMetaError(err error) error {
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
}
