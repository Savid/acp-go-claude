package claude

import (
	"errors"
	"fmt"
	"strings"
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
)

// ProcessExitError describes an unexpected Claude subprocess exit and preserves
// the real cause: the exit status and a tail of the process stderr. It is the
// error surfaced when the Claude process dies mid-turn so the ACP client sees
// the true cause instead of a fixed placeholder.
type ProcessExitError struct {
	// ExitCode is the process exit status, or -1 when it cannot be determined.
	ExitCode int
	// StderrTail holds the last lines the process emitted on stderr.
	StderrTail string
	// Err is the underlying wait error.
	Err error
}

// Error renders the exit status, wait error, and stderr tail as one string.
func (e *ProcessExitError) Error() string {
	var b strings.Builder

	b.WriteString("claude exited")

	if e.ExitCode >= 0 {
		fmt.Fprintf(&b, " with status %d", e.ExitCode)
	}

	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}

	if tail := strings.TrimSpace(e.StderrTail); tail != "" {
		fmt.Fprintf(&b, "; stderr: %s", tail)
	}

	return b.String()
}

// Unwrap exposes the underlying wait error.
func (e *ProcessExitError) Unwrap() error { return e.Err }

// Is reports the error as an ErrProcessExited so callers can classify process
// death without depending on the concrete type.
func (e *ProcessExitError) Is(target error) bool { return target == ErrProcessExited }
