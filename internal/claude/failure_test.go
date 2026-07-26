package claude

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
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

	boom := &ProcessExitError{ExitCode: 1, StderrTail: "boom", Err: errors.New("signal: killed")}
	transport.errs <- boom

	msg, err := client.Receive(context.Background())
	require.Nil(t, msg)
	require.ErrorIs(t, err, ErrProcessExited)
	require.ErrorIs(t, err, boom)
}

func TestProcessExitErrorIs(t *testing.T) {
	t.Parallel()

	require.True(t, errors.Is(&ProcessExitError{}, ErrProcessExited))
	require.False(t, errors.Is(&ProcessExitError{}, ErrClientClosed))
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
	transport.errs <- &ProcessExitError{ExitCode: 1, Err: errors.New("boom")}
	require.Eventually(t, func() bool { return !client.Alive() }, time.Second, 5*time.Millisecond)

	require.NoError(t, client.Close())
	require.False(t, client.Alive())
}

func TestProcessTransportStderrTailBounded(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{}
	for i := range stderrTailLines + 10 {
		transport.appendStderr("line-" + strconv.Itoa(i))
	}

	lines := strings.Split(transport.StderrTail(), "\n")
	require.Len(t, lines, stderrTailLines)
	require.Equal(t, "line-"+strconv.Itoa(stderrTailLines+9), lines[len(lines)-1])
}

func TestProcessTransportProcessExitErrorPlainCause(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{}
	transport.appendStderr("disk full")

	err := transport.processExitError(errors.New("write: broken pipe"))

	var exit *ProcessExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, -1, exit.ExitCode)
	require.Equal(t, "disk full", exit.StderrTail)
	require.Contains(t, err.Error(), "write: broken pipe")
	require.Contains(t, err.Error(), "disk full")
}

func TestProcessTransportMalformedLineLogged(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		log:    slog.New(slog.DiscardHandler),
		stdout: io.NopCloser(strings.NewReader("{bad\n{\"type\":\"assistant\"}\n")),
	}

	messages, errs := transport.Messages(context.Background())

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

	exit := &ProcessExitError{
		ExitCode:   1,
		StderrTail: "No conversation found with session ID: 11111111-1111-4111-8111-111111111111",
		Err:        errors.New("exit status 1"),
	}
	transport.errs <- exit

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller stop")
	}

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.ErrorContains(t, err, "claude control controller stopped")
	require.ErrorIs(t, err, ErrProcessExited)
	require.ErrorIs(t, err, exit)
	require.Contains(t, err.Error(), "No conversation found with session ID")
}

func TestControllerSendRequestStopWithoutCause(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)
	startControllerForTest(t, controller, context.Background())

	close(transport.incoming)

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller stop")
	}

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.EqualError(t, err, "claude control controller stopped")
}

func TestControllerDrainErrorsRecordsQueuedCause(t *testing.T) {
	t.Parallel()

	controller := NewController(slog.New(slog.DiscardHandler), newFakeTransport())

	errs := make(chan error, 3)
	errs <- nil
	errs <- errors.New("first")
	errs <- errors.New("second")
	close(errs)

	controller.drainErrors(context.Background(), errs)
	require.EqualError(t, controller.LastError(), "first")

	empty := NewController(slog.New(slog.DiscardHandler), newFakeTransport())
	empty.drainErrors(context.Background(), make(chan error))
	require.NoError(t, empty.LastError())
}

// The transport queues the exit cause and then closes both channels, so the
// router can observe either the closed message channel or the queued error
// first. Both orders must keep the cause.
func TestControllerRecordsCauseWhenMessageChannelClosesFirst(t *testing.T) {
	t.Parallel()

	for range 32 {
		transport := newFakeTransport()
		transport.errs <- &ProcessExitError{ExitCode: 1, Err: errors.New("exit status 1")}
		close(transport.incoming)

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
