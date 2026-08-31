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

	nativeTransportFailureMessage = "claude transport failed"

	authLoginMarker = "Please run /login"
)

// turnFailureError builds the uniform -32603 claude_turn_failed error with a
// stable, adapter-owned message. Provider payloads and paths never cross it.
func turnFailureError(cause string, message string) *acp.RequestError {
	return acp.NewInternalError(map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: cause,
		jsonFieldMessage:  message,
	})
}

// nativeTurnFailure classifies an error observed while reading the Claude turn
// stream into the uniform failure shape. Process death maps to process_exit with
// only its closed status classification; provider stderr is never retained or
// returned. Everything else maps to the transport classification.
func nativeTurnFailure(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) {
		return turnFailureError(failureCauseTransport, context.Canceled.Error())
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return turnFailureError(failureCauseTransport, context.DeadlineExceeded.Error())
	}

	var exit *claude.ProcessExitError
	if errors.As(err, &exit) {
		return turnFailureError(failureCauseProcessExit, exit.Error())
	}

	if errors.Is(err, claude.ErrProcessExited) {
		return turnFailureError(failureCauseProcessExit, claude.ErrProcessExited.Error())
	}

	if errors.Is(err, ErrContainmentIncomplete) {
		return turnFailureError(failureCauseTransport, ErrContainmentIncomplete.Error())
	}

	return turnFailureError(failureCauseTransport, nativeTransportFailureMessage)
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
		if errors.Is(abortErr, ErrContainmentIncomplete) {
			return acp.PromptResponse{}, nativeTurnFailure(abortErr)
		}

		return acp.PromptResponse{}, s.turnTimeoutFailure(s.agent.turnTimeout().String())
	}

	if turnCtx.Err() != nil {
		abortErr := s.cancelNative(ctx)
		if errors.Is(abortErr, ErrContainmentIncomplete) {
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
// failure — is -32603 with cause "provider": this adapter advertises no
// agent-level ACP auth methods, and the -32000 variant of this envelope is
// scoped to a sibling that does. The session-scoped provider-auth legs are a
// separate surface with an error shape of their own. The singular result error
// field is parsed so the provider cause is not lost when result is empty.
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
		jsonFieldMessage:  "claude provider turn failed",
	}

	return acp.NewInternalError(data)
}

func isProviderAuthError(result *claude.ResultMessage) bool {
	return strings.Contains(result.Result, authLoginMarker) ||
		strings.Contains(result.Error, authLoginMarker)
}
