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

// authTurnFailureError builds the -32000 variant used when the native cause is
// an authentication failure. It keeps the uniform data shape.
func authTurnFailureError(message string) *acp.RequestError {
	return acp.NewAuthRequired(map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: failureCauseProvider,
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
// The cancel guard runs before all failure mapping: a native error observed
// while the turn is cancelled maps to cancelled, never a failure. A turn timeout
// is a failure, not a cancel, so it is checked first.
func (s *agentSession) receiveTurnFailure(
	ctx context.Context,
	turnCtx context.Context,
	messageID *string,
	err error,
	timedOut bool,
) (acp.PromptResponse, error) {
	if timedOut {
		return acp.PromptResponse{}, s.turnTimeoutFailure(ctx, s.agent.turnTimeout().String())
	}

	if turnCtx.Err() != nil {
		// Cancellation suppresses the native error by design: a cancelled turn is
		// a successful PromptResponse, not a failure.
		cancelled := acp.PromptResponse{
			StopReason:    acp.StopReasonCancelled,
			UserMessageId: messageID,
		}

		return cancelled, nil //nolint:nilerr // cancellation intentionally suppresses the native error
	}

	if isNativeProcessExit(err) {
		s.agent.observe.RecordClaudeProcessExit(ctx, "unexpected", err)
	}

	return acp.PromptResponse{}, nativeTurnFailure(s.interruptAfterEmitError(ctx, err))
}

// turnTimeoutFailure aborts the native turn and returns the timeout failure. A
// timeout is a failure, not a user cancel, so it never maps to cancelled.
func (s *agentSession) turnTimeoutFailure(ctx context.Context, timeout string) error {
	_ = s.interruptAfterEmitError(ctx, nil)

	return turnFailureError(failureCauseTimeout, fmt.Sprintf("claude turn exceeded %s", timeout))
}

// providerTurnFailure maps a Claude result frame that reports an error into the
// uniform failure shape. Auth failures keep -32000; every other provider error
// is -32603 with cause "provider". The singular result error field is parsed so
// the provider cause is not lost when result is empty.
func providerTurnFailure(result *claude.ResultMessage, assistantErrorKind string) error {
	if result == nil {
		return nil
	}

	if isProviderAuthError(result) {
		return authTurnFailureError(providerErrorMessage(result))
	}

	if result.StopReason == stopReasonMaxTokens || !result.IsError {
		return nil
	}

	data := map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: failureCauseProvider,
		jsonFieldMessage:  providerErrorMessage(result),
		jsonFieldSubtype:  result.Subtype,
	}

	if len(result.Errors) > 0 {
		data["errors"] = append([]string(nil), result.Errors...)
	}

	if assistantErrorKind != "" {
		data["errorKind"] = assistantErrorKind
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
