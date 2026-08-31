package claude

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientReceiveSurfacesLastError(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitialize(transport)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() { _ = client.Close() })

	boom := &ProcessExitError{ExitCode: 1}
	transport.sendError(boom)
	close(transport.events)

	msg, err := client.Receive(context.Background())
	require.Nil(t, msg)
	require.ErrorIs(t, err, ErrProcessExited)
	var exit *ProcessExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode)
}

func TestProcessExitErrorIs(t *testing.T) {
	t.Parallel()

	require.True(t, errors.Is(&ProcessExitError{}, ErrProcessExited))
	require.False(t, errors.Is(&ProcessExitError{}, ErrClientClosed))
}

func TestClosedTransportErrorPreservesRecognizedJoinedCausesWithoutOpaqueText(t *testing.T) {
	t.Parallel()

	const secret = "provider-secret-terminal-cause"
	err := closedTransportError(errors.Join(
		context.Canceled,
		errClaudeStdoutRead,
		&ProcessExitError{ExitCode: 23},
		errors.New(secret),
	))

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errClaudeStdoutRead)
	require.ErrorIs(t, err, ErrProcessExited)
	var exit *ProcessExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 23, exit.ExitCode)
	require.NotContains(t, err.Error(), secret)
}

func TestClosedTransportErrorClassifiesEveryTerminalBoundary(t *testing.T) {
	t.Parallel()

	require.NoError(t, closedTransportError(nil))

	dataFailure := &ControllerDataError{Kind: ControllerDataTeardownAbort}
	classified := closedTransportError(dataFailure)
	var classifiedData *ControllerDataError
	require.ErrorAs(t, classified, &classifiedData)
	require.Equal(t, dataFailure.Kind, classifiedData.Kind)
	require.NotSame(t, dataFailure, classifiedData)

	bareExit := closedTransportError(ErrProcessExited)
	require.ErrorIs(t, bareExit, ErrProcessExited)
	var concreteExit *ProcessExitError
	require.False(t, errors.As(bareExit, &concreteExit))

	const opaque = "opaque-provider-failure"
	opaqueFailure := closedTransportError(errors.New(opaque))
	require.Error(t, opaqueFailure)
	require.NotContains(t, opaqueFailure.Error(), opaque)

	tests := []struct {
		name  string
		err   error
		class string
	}{
		{name: "none", class: transportClassNone},
		{name: "canceled", err: context.Canceled, class: transportClassCanceled},
		{name: "deadline", err: context.DeadlineExceeded, class: transportClassDeadline},
		{name: "process exit", err: ErrProcessExited, class: "process_exit"},
		{name: "stdout panic", err: errClaudeStdoutReaderPanic, class: "stdout_panic"},
		{name: "stdout read", err: errClaudeStdoutRead, class: "stdout_read"},
		{name: "response write", err: errControlResponseWrite, class: "response_write"},
		{name: "transport", err: errClaudeTransportFailure, class: transportClassFailure},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.class, transportErrorClass(test.err))
		})
	}
}

func TestClientAlive(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	// Not started yet: no controller.
	require.False(t, client.Alive())

	go autoRespondInitialize(transport)
	require.NoError(t, client.Start(context.Background()))
	require.True(t, client.Alive())

	// The native process dies: the controller stops and the client is no longer
	// alive, so callers can relaunch lazily.
	transport.sendError(&ProcessExitError{ExitCode: 1})
	close(transport.events)
	require.Eventually(t, func() bool { return !client.Alive() }, time.Second, 5*time.Millisecond)

	require.NoError(t, client.Close())
	require.False(t, client.Alive())
}

func TestProcessTransportMalformedLineLogged(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		log:    slog.New(slog.DiscardHandler),
		stdout: io.NopCloser(strings.NewReader("{bad\n{\"type\":\"assistant\"}\n")),
	}

	messages, errs := splitEventsForTest(transport.Events(context.Background()))

	var got []map[string]any
	for msg := range messages {
		got = append(got, msg)
	}

	require.Len(t, got, 1)
	require.Equal(t, MessageTypeAssistant, got[0][keyType])
	require.Equal(t, uint64(1), transport.malformedLines.Load())

	_, ok := <-errs
	require.False(t, ok)
}

func TestControllerSendRequestSurfacesStopCause(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)
	startControllerForTest(t, controller, context.Background())

	exit := &ProcessExitError{ExitCode: 1}
	transport.sendError(exit)
	close(transport.events)

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller stop")
	}

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.ErrorContains(t, err, "claude control controller stopped")
	require.ErrorIs(t, err, ErrProcessExited)
	require.NotContains(t, err.Error(), "No conversation found with session ID")
}

func TestControllerSendRequestStopWithoutCause(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)
	startControllerForTest(t, controller, context.Background())

	close(transport.events)

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller stop")
	}

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.EqualError(t, err, "claude control controller stopped")
}

func TestControllerRecordsOrderedTerminalCause(t *testing.T) {
	t.Parallel()

	for range 32 {
		transport := newFakeTransport()
		transport.sendError(&ProcessExitError{ExitCode: 1})
		close(transport.events)

		controller := NewController(slog.New(slog.DiscardHandler), transport)
		startControllerForTest(t, controller, context.Background())

		select {
		case <-controller.Done():
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for controller stop")
		}

		require.ErrorIs(t, controller.LastError(), ErrProcessExited)
	}
}
