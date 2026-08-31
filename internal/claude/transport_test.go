package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type transportBuffer struct {
	bytes.Buffer
	closed bool
}

func (w *transportBuffer) Close() error {
	w.closed = true

	return nil
}

type transportErrorWriter struct{}

func (transportErrorWriter) Write([]byte) (int, error) { return 0, errors.New("secret write failure") }
func (transportErrorWriter) Close() error              { return nil }

type transportErrorReader struct{ err error }

func (r transportErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (transportErrorReader) Close() error               { return nil }

type transportPanicReader struct{}

func (transportPanicReader) Read([]byte) (int, error) { panic("secret panic") }
func (transportPanicReader) Close() error             { return nil }

type transportDeadlineWriter struct {
	transportBuffer
	mu        sync.Mutex
	deadlines []time.Time
}

func (w *transportDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()

	return nil
}

func TestProcessTransportSendCurrentBehavior(t *testing.T) {
	stdin := &transportBuffer{}
	transport := &ProcessTransport{stdin: stdin}
	require.NoError(t, transport.Send(t.Context(), map[string]any{"type": "user"}))
	require.JSONEq(t, `{"type":"user"}`, strings.TrimSpace(stdin.String()))

	require.Error(t, (&ProcessTransport{}).Send(t.Context(), map[string]any{}))
	require.ErrorIs(t, transport.Send(t.Context(), map[string]any{"bad": func() {}}), errClaudePayloadMarshal)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, transport.Send(cancelled, map[string]any{}), context.Canceled)

	transport.stdin = transportErrorWriter{}
	require.ErrorIs(t, transport.Send(t.Context(), map[string]any{}), errClaudeStdinWrite)
}

func TestProcessTransportSendBoundsAndDeadline(t *testing.T) {
	priorLimit := maxJSONLineBytes
	maxJSONLineBytes = 12
	t.Cleanup(func() { maxJSONLineBytes = priorLimit })

	stdin := &transportDeadlineWriter{}
	transport := &ProcessTransport{stdin: stdin}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	require.NoError(t, transport.Send(ctx, map[string]any{"x": "y"}))
	require.Equal(t, []time.Time{deadline, {}}, stdin.deadlines)
	require.ErrorContains(t, transport.Send(t.Context(), map[string]any{"type": "oversized"}), "exceeds")
}

func TestProcessTransportEventsSkipMalformedWithoutLeakingBytes(t *testing.T) {
	var logs bytes.Buffer
	transport := &ProcessTransport{
		log:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		stdout: io.NopCloser(strings.NewReader("noise\n{secret malformed\n{\"type\":\"assistant\"}\n{\"type\":\"result\"}\n")),
	}
	messages, errs := splitEventsForTest(transport.Events(t.Context()))
	var got []map[string]any
	for message := range messages {
		got = append(got, message)
	}
	for err := range errs {
		t.Fatalf("unexpected transport error: %v", err)
	}

	require.Len(t, got, 2)
	require.Equal(t, "assistant", got[0]["type"])
	require.Equal(t, "result", got[1]["type"])
	require.Equal(t, uint64(1), transport.malformedLines.Load())
	require.Contains(t, logs.String(), "malformed_lines=1")
	require.NotContains(t, logs.String(), "secret malformed")
}

func TestProcessTransportEventsNormalizeReaderAndExitFailures(t *testing.T) {
	transport := &ProcessTransport{stdout: transportErrorReader{err: errors.New("secret read failure")}}
	_, errs := splitEventsForTest(transport.Events(t.Context()))
	err := <-errs
	require.ErrorIs(t, err, errClaudeStdoutRead)
	require.NotContains(t, err.Error(), "secret read failure")

	transport = &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("")),
		process: &authorityTestProcess{
			stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader("")),
			wait:   func(context.Context) (NativeResult, error) { return NativeResult{ExitCode: 7}, nil },
			revoke: func(context.Context) error { return nil },
		},
	}
	_, errs = splitEventsForTest(transport.Events(t.Context()))
	err = <-errs
	require.ErrorIs(t, err, ErrProcessExited)
	var exit *ProcessExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 7, exit.ExitCode)

	transport = &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("")),
		process: &authorityTestProcess{
			stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader("")),
			wait: func(context.Context) (NativeResult, error) {
				return NativeResult{ExitCode: -1, Signal: 15, Revoked: true}, nil
			},
			revoke: func(context.Context) error { return nil },
		},
	}
	_, errs = splitEventsForTest(transport.Events(t.Context()))
	err = <-errs
	require.ErrorAs(t, err, &exit)
	require.Equal(t, -1, exit.ExitCode)
	require.Equal(t, 15, exit.Signal)
	require.True(t, exit.Revoked)
}

func TestProcessTransportEventsRecoverReaderPanics(t *testing.T) {
	transport := &ProcessTransport{log: slog.New(slog.DiscardHandler), stdout: transportPanicReader{}}
	_, errs := splitEventsForTest(transport.Events(t.Context()))
	err := <-errs
	require.ErrorIs(t, err, errClaudeStdoutReaderPanic)
	require.NotContains(t, err.Error(), "secret panic")

	transport = &ProcessTransport{
		log:    slog.New(slog.DiscardHandler),
		stdout: io.NopCloser(strings.NewReader("")),
		stderr: transportPanicReader{},
	}
	_, errs = splitEventsForTest(transport.Events(t.Context()))
	err = <-errs
	require.ErrorIs(t, err, errClaudeTransportFailure)
	require.NotContains(t, err.Error(), "secret panic")
}

func TestProcessTransportEventsSingleUseAndClosed(t *testing.T) {
	transport := &ProcessTransport{stdout: io.NopCloser(strings.NewReader("{\"type\":\"result\"}\n"))}
	first := transport.Events(t.Context())
	second := transport.Events(t.Context())
	require.ErrorIs(t, (<-second).Err, errEventsAlreadyStarted)
	for range first {
	}

	closed := &ProcessTransport{stdout: io.NopCloser(strings.NewReader(""))}
	require.NoError(t, closed.Close())
	require.ErrorIs(t, (<-closed.Events(t.Context())).Err, ErrClientClosed)
}

func TestProcessTransportEventsRejectOversizedLine(t *testing.T) {
	priorLimit := maxJSONLineBytes
	maxJSONLineBytes = 128
	t.Cleanup(func() { maxJSONLineBytes = priorLimit })
	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader(`{"type":"` + strings.Repeat("x", maxJSONLineBytes+1) + `"}` + "\n")),
	}
	messages, errs := splitEventsForTest(transport.Events(t.Context()))
	require.Empty(t, <-messages)
	err := <-errs
	require.ErrorIs(t, err, errClaudeStdoutRead)
}
