package claude

import (
	"context"
	"errors"
	"fmt"
)

const (
	transportClassNone        = "none"
	transportClassCanceled    = "canceled"
	transportClassDeadline    = "deadline"
	transportClassContainment = "containment"
	transportClassFailure     = "transport"
)

var (
	// ErrClientClosed indicates the Claude client has already been closed.
	ErrClientClosed = errors.New("claude client is closed")
	// ErrClientNotStarted indicates a method needs a running Claude process.
	ErrClientNotStarted = errors.New("claude client is not started")
	// ErrMessageStreamClosed indicates Claude's message stream ended.
	ErrMessageStreamClosed = errors.New("claude message stream closed")
	// ErrProcessExited indicates the Claude subprocess exited.
	ErrProcessExited = errors.New("claude exited")
	// ErrSessionNotFound indicates Claude could not find a requested session.
	ErrSessionNotFound = errors.New("claude session not found")
	// ErrQueryClosed indicates Claude closed a query before producing a response.
	ErrQueryClosed = errors.New("claude query closed")

	errClaudeTransportFailure   = errors.New("claude transport failed")
	errClaudePayloadMarshal     = errors.New("claude transport payload encoding failed")
	errClaudeStdinWrite         = errors.New("claude stdin write failed")
	errClaudeStdoutRead         = errors.New("claude stdout reader failed")
	errClaudeStdoutReaderPanic  = errors.New("claude stdout reader panicked")
	errControlHandlerPanic      = errors.New("claude control handler panicked")
	errControlHandlerFailure    = errors.New("claude control handler failed")
	errControlResponseWrite     = errors.New("claude control response write failed")
	errClaudeControlRequestFail = errors.New("claude control request failed")
)

// ControllerDataFailureKind identifies how the controller stopped delivering
// frames already admitted from the native source.
type ControllerDataFailureKind string

const (
	// ControllerDataOverflow means the controller's bounded admitted-frame queue
	// filled before the consumer advanced it.
	ControllerDataOverflow ControllerDataFailureKind = "overflow"
	// ControllerDataTeardownAbort means deliberate client teardown interrupted
	// delivery. Source EOF never uses this cause and always drains completely.
	ControllerDataTeardownAbort ControllerDataFailureKind = "teardown_abort"
)

// ControllerDataError is a secret-safe typed controller generation failure.
// It carries classification only; native frames and provider error text never
// enter its rendering.
type ControllerDataError struct {
	Kind ControllerDataFailureKind
}

func (e *ControllerDataError) Error() string {
	return "claude controller data delivery failed: " + string(e.Kind)
}

// transportFailure keeps the exact causal identity available to errors.Is
// while rendering only the adapter-owned transport classification. In
// particular, provider, filesystem, and payload text carried by cause never
// becomes part of a returned or logged error string.
type classifiedTransportError struct{ cause error }

func (e *classifiedTransportError) Error() string { return errClaudeTransportFailure.Error() }

func (e *classifiedTransportError) Unwrap() []error {
	return []error{errClaudeTransportFailure, e.cause}
}

// ProcessExitError describes an unexpected Claude subprocess exit using only
// the closed status classification safe to expose outside the transport.
type ProcessExitError struct {
	// ExitCode is the process exit status, or -1 when it cannot be determined.
	ExitCode int
}

// Error renders only the process status. Provider stderr and wait error bodies
// are deliberately not retained or returned.
func (e *ProcessExitError) Error() string {
	if e.ExitCode >= 0 {
		return fmt.Sprintf("claude exited with status %d", e.ExitCode)
	}

	return ErrProcessExited.Error()
}

// Is reports the error as an ErrProcessExited so callers can classify process
// death without depending on the concrete type.
func (e *ProcessExitError) Is(target error) bool { return target == ErrProcessExited }

func closedTransportError(err error) error {
	if err == nil {
		return nil
	}

	closed := make([]error, 0, 8)

	var dataFailure *ControllerDataError
	if errors.As(err, &dataFailure) {
		closed = append(closed, &ControllerDataError{Kind: dataFailure.Kind})
	}

	for _, recognized := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		ErrProcessContainmentIncomplete,
		ErrClientClosed,
		ErrClientNotStarted,
		ErrMessageStreamClosed,
		errClaudeTransportFailure,
		errClaudePayloadMarshal,
		errClaudeStdinWrite,
		errClaudeStdoutRead,
		errClaudeStdoutReaderPanic,
		errControlHandlerPanic,
		errControlHandlerFailure,
		errControlResponseWrite,
		errClaudeControlRequestFail,
	} {
		if errors.Is(err, recognized) {
			closed = append(closed, recognized)
		}
	}

	if errors.Is(err, ErrProcessExited) {
		var exit *ProcessExitError
		if errors.As(err, &exit) {
			closed = append(closed, &ProcessExitError{ExitCode: exit.ExitCode})
		} else {
			closed = append(closed, ErrProcessExited)
		}
	}

	if len(closed) == 0 {
		return &classifiedTransportError{cause: err}
	}

	return errors.Join(closed...)
}

func transportErrorClass(err error) string {
	switch {
	case err == nil:
		return transportClassNone
	case errors.Is(err, context.Canceled):
		return transportClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return transportClassDeadline
	case errors.Is(err, ErrProcessContainmentIncomplete):
		return transportClassContainment
	case errors.Is(err, ErrProcessExited):
		return "process_exit"
	case errors.Is(err, errClaudeStdoutReaderPanic):
		return "stdout_panic"
	case errors.Is(err, errClaudeStdoutRead):
		return "stdout_read"
	case errors.Is(err, errControlResponseWrite):
		return "response_write"
	default:
		return transportClassFailure
	}
}
