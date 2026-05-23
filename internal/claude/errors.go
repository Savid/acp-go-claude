package claude

import "errors"

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
