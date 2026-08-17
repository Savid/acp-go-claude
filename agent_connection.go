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

// requestError maps a handler failure onto the wire error the peer receives.
//
// Cancellation is read off the request context, not off the error. An honored
// $/cancel_request is the only thing that cancels a request context with cause
// context.Canceled: connection teardown cancels the parent with the transport
// cause, and an adapter deadline yields context.DeadlineExceeded, so neither
// becomes -32800 and a deadline stays an internal failure. Asking the error
// instead answers a different question — what the handler happened to return —
// which both misses a cancel the handler swallowed and reports -32800 for a
// request nobody cancelled but whose error merely wrapped context.Canceled.
//
// The cause is consulted first because work aborted by a cancel routinely
// carries a typed RequestError out with it. Passing that through would tell a
// peer that withdrew its request that its parameters were bad; a cancelled
// request has no honest answer other than -32800.
func requestError(ctx context.Context, err error) *acp.RequestError {
	if err == nil {
		return nil
	}

	if context.Cause(ctx) == context.Canceled {
		return acp.NewRequestCancelled(map[string]any{jsonFieldError: err.Error()})
	}

	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: err.Error()})
}
