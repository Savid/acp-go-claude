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
	CreateElicitation(context.Context, acp.UnstableCreateElicitationRequest, elicitationScope, actionWireAdmission) (acp.UnstableCreateElicitationResponse, error)
	ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error)
	WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error)
	RequestPermission(context.Context, acp.RequestPermissionRequest, actionWireAdmission) (acp.RequestPermissionResponse, error)
	SessionUpdate(context.Context, acp.SessionNotification) error
	CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error)
	KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error)
	TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error)
	ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error)
	WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error)
	NotifyExtension(context.Context, string, any) error
}

// actionWireAdmission binds a prepared lifecycle action to the actual JSON-RPC
// request that carries its correlation. Production calls written only after the
// registered request line has been written in full; test clients must cross the
// same boundary explicitly.
type actionWireAdmission struct {
	actionID string
	publish  func() error
	written  func(context.Context, actionWireIdentity) error
}

type actionWireIdentity struct {
	method    string
	requestID string
}

func (a actionWireAdmission) present() bool {
	return a.written != nil
}

func (a actionWireAdmission) observeWrite(ctx context.Context, identity actionWireIdentity) error {
	if !a.present() {
		return nil
	}

	return a.written(ctx, identity)
}

func (a actionWireAdmission) publishPending() error {
	if a.publish == nil {
		return nil
	}

	return a.publish()
}

type elicitationScope struct {
	SessionID  acp.SessionId
	TurnNonce  string
	ToolCallID acp.ToolCallId
	RequestID  *string
}

// safeRequestError keeps the original causal identity available to errors.Is
// while exposing only adapter-owned structured data to JSON-RPC and logs.
type safeRequestError struct {
	request *acp.RequestError
	cause   error
}

func newSafeRequestFailure(request *acp.RequestError, cause error) error {
	return &safeRequestError{request: request, cause: cause}
}

func (e *safeRequestError) Error() string {
	return e.request.Error()
}

func (e *safeRequestError) Unwrap() error {
	return e.cause
}

func (e *safeRequestError) As(target any) bool {
	request, ok := target.(**acp.RequestError)
	if !ok {
		return false
	}

	*request = e.request

	return true
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
		return acp.NewRequestCancelled(map[string]any{jsonFieldError: "request_cancelled"})
	}

	if context.Cause(ctx) == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return acp.NewInternalError(map[string]any{
			jsonFieldError: "request_deadline_exceeded",
			"class":        "deadline",
		})
	}

	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInternalError(map[string]any{
		jsonFieldError: "claude_internal_failure",
		"class":        safeErrorClass(err),
	})
}

func safeErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "internal"
	}
}
