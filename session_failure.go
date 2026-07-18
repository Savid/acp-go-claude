package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// Native turn failures share one uniform wire shape so hosts can classify a
// failed turn without vendor-specific parsing. A native turn failure terminates
// session/prompt with a JSON-RPC error and no PromptResponse; it is never
// reported as a stop reason.
const (
	turnFailedError = "claude_turn_failed"

	failureFieldCause = "cause"

	failureCauseProcessExit = "process_exit"
	failureCauseTransport   = "transport"
	failureCauseProvider    = "provider"
	failureCauseTimeout     = "timeout"

	authLoginMarker = "Please run /login"
)

// turnFailureError builds the uniform -32603 claude_turn_failed error with the
// given machine-readable cause and the real native cause text.
func turnFailureError(cause string, message string) *acp.RequestError {
	return acp.NewInternalError(map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: cause,
		jsonFieldMessage:  message,
	})
}

// nativeTurnFailure classifies an error observed while reading the Claude turn
// stream into the uniform failure shape. Process death maps to process_exit
// (with the real exit status and stderr tail); everything else (stream close,
// malformed frame, client lifecycle) maps to transport. It never surfaces a
// fixed placeholder or a bare stream-closed sentinel.
func nativeTurnFailure(err error) error {
	if err == nil {
		return nil
	}

	var exit *claude.ProcessExitError
	if errors.As(err, &exit) {
		return turnFailureError(failureCauseProcessExit, exit.Error())
	}

	if errors.Is(err, claude.ErrProcessExited) {
		return turnFailureError(failureCauseProcessExit, err.Error())
	}

	return turnFailureError(failureCauseTransport, err.Error())
}

// isNativeProcessExit reports whether err is a Claude process-death error, used
// for observability and lazy relaunch decisions.
func isNativeProcessExit(err error) bool {
	return errors.Is(err, claude.ErrProcessExited) ||
		errors.Is(err, claude.ErrMessageStreamClosed) ||
		errors.Is(err, claude.ErrClientNotStarted)
}

// receiveTurnFailure classifies an error observed while reading the turn stream.
// The cancel guard runs strictly before all failure mapping — including timeout
// expiry. A user cancel and a WithTurnTimeout expiry can coincide because both
// cancel turnCtx, so an explicit user cancel is checked first and wins
// deterministically: the turn resolves cancelled, cause:"timeout" is never
// emitted, and the timeout failure path (which would abort the native turn a
// second time) never runs. Only once no user cancel is in flight does a timeout
// map to a failure; a bare parent-context cancellation still maps to cancelled.
func (s *agentSession) receiveTurnFailure(
	ctx context.Context,
	turnCtx context.Context,
	messageID *string,
	err error,
	timedOut bool,
) (acp.PromptResponse, error) {
	if s.wasTurnCancelled() {
		// A user cancel already aborted the native turn; suppress the native error
		// and resolve cancelled even when a turn timeout expired at the same time.
		return s.cancelledResponse(messageID), nil
	}

	if timedOut {
		return acp.PromptResponse{}, s.turnTimeoutFailure(s.agent.turnTimeout().String())
	}

	if turnCtx.Err() != nil {
		// Parent-context cancellation suppresses the native error by design: a
		// cancelled turn is a successful PromptResponse, not a failure.
		return s.cancelledResponse(messageID), nil //nolint:nilerr // cancellation intentionally suppresses the native error
	}

	if isNativeProcessExit(err) {
		s.agent.observe.RecordClaudeProcessExit(ctx, "unexpected", err)
	}

	return acp.PromptResponse{}, nativeTurnFailure(s.interruptAfterEmitError(ctx, err))
}

// cancelledResponse builds the successful cancelled PromptResponse echoing the
// user message id.
func (s *agentSession) cancelledResponse(messageID *string) acp.PromptResponse {
	return acp.PromptResponse{
		StopReason:    acp.StopReasonCancelled,
		UserMessageId: messageID,
	}
}

// turnTimeoutFailure returns the timeout failure. Prompt's settlement fence
// has already completed the selected containment boundary before this error is
// allowed to return.
func (s *agentSession) turnTimeoutFailure(timeout string) error {
	return turnFailureError(failureCauseTimeout, fmt.Sprintf("claude turn exceeded %s", timeout))
}

// settlePromptTurn runs with cancelMu held by Prompt's deferred turn cleanup.
// It is the single settlement fence for explicit cancellation, parent-context
// cancellation, and WithTurnTimeout expiry: no response can return before the
// selected containment boundary completes.
func (s *agentSession) settlePromptTurn(
	ctx context.Context,
	turnCtx context.Context,
	messageID *string,
	timedOut bool,
	response acp.PromptResponse,
	promptErr error,
) (acp.PromptResponse, error) {
	s.mu.Lock()
	cancelled := s.turnCancelled
	containmentErr := s.turnContainmentErr
	s.mu.Unlock()

	if cancelled {
		if containmentErr != nil {
			return acp.PromptResponse{}, nativeTurnFailure(containmentErr)
		}

		if promptErr == nil {
			return s.cancelledResponse(messageID), nil
		}

		return response, promptErr
	}

	if timedOut {
		abortErr := s.cancelNative(ctx)
		if errors.Is(abortErr, claude.ErrProcessContainmentIncomplete) {
			return acp.PromptResponse{}, nativeTurnFailure(abortErr)
		}

		return acp.PromptResponse{}, s.turnTimeoutFailure(s.agent.turnTimeout().String())
	}

	if turnCtx.Err() != nil {
		abortErr := s.cancelNative(ctx)
		if errors.Is(abortErr, claude.ErrProcessContainmentIncomplete) {
			return acp.PromptResponse{}, nativeTurnFailure(abortErr)
		}

		if promptErr != nil {
			return response, promptErr
		}

		return s.cancelledResponse(messageID), nil
	}

	return response, promptErr
}

// providerTurnFailure maps a Claude result frame that reports an error into the
// uniform failure shape. Every provider error — including an authentication
// failure — is -32603 with cause "provider"; this adapter advertises no ACP
// auth methods, so it never emits the -32000 AuthRequired variant. The singular
// result error field is parsed so the provider cause is not lost when result is
// empty.
func providerTurnFailure(result *claude.ResultMessage) error {
	if result == nil {
		return nil
	}

	authFailure := isProviderAuthError(result)

	if result.StopReason == stopReasonMaxTokens || (!result.IsError && !authFailure) {
		return nil
	}

	data := map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: failureCauseProvider,
		jsonFieldMessage:  providerErrorMessage(result),
	}

	if result.Subtype != "" {
		data[jsonFieldSubtype] = result.Subtype
	}

	return acp.NewInternalError(data)
}

// providerErrorMessage returns the best available native cause text from a
// result frame, preferring result, then the singular error field, then joined
// errors, then the subtype. It never returns an empty placeholder.
func providerErrorMessage(result *claude.ResultMessage) string {
	if text := strings.TrimSpace(result.Result); text != "" {
		return result.Result
	}

	if text := strings.TrimSpace(result.Error); text != "" {
		return result.Error
	}

	if len(result.Errors) > 0 {
		return strings.Join(result.Errors, "; ")
	}

	if result.Subtype != "" {
		return result.Subtype
	}

	return "claude reported a turn error"
}

func isProviderAuthError(result *claude.ResultMessage) bool {
	return strings.Contains(result.Result, authLoginMarker) ||
		strings.Contains(result.Error, authLoginMarker)
}
