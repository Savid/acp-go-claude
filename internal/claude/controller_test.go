package claude

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeTransport struct {
	incoming chan map[string]any
	errs     chan error

	mu   sync.Mutex
	sent []any

	sendErr  error
	closeErr error
	closed   bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		incoming: make(chan map[string]any, 16),
		errs:     make(chan error, 1),
	}
}

func (f *fakeTransport) Start(context.Context) error { return nil }

func (f *fakeTransport) Send(_ context.Context, payload any) error {
	f.mu.Lock()
	f.sent = append(f.sent, payload)
	err := f.sendErr
	f.mu.Unlock()

	return err
}

func (f *fakeTransport) Messages(context.Context) (<-chan map[string]any, <-chan error) {
	return f.incoming, f.errs
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return f.closeErr
}

func (f *fakeTransport) sentPayloads() []any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]any(nil), f.sent...)
}

func (f *fakeTransport) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

func startControllerForTest(t *testing.T, controller *Controller, parent context.Context) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(parent)
	controller.Start(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-controller.Done():
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for controller cleanup")
		}
	})

	return ctx
}

func TestControllerRecoverRouterClosesTransport(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)

	func() {
		defer controller.recoverRouter(context.Background())

		panic("boom")
	}()

	require.True(t, transport.isClosed())
}

func TestControllerRecoverControlRequestStopsAndClosesTransport(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)

	func() {
		defer controller.recoverControlRequest(context.Background())

		panic("boom")
	}()

	require.True(t, transport.isClosed())

	select {
	case <-controller.Done():
	default:
		t.Fatal("controller was not stopped")
	}
}

func TestControllerRoutesDataMessages(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{"type": "assistant"}

	select {
	case msg := <-controller.Messages():
		require.Equal(t, "assistant", msg["type"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for data message")
	}
}

func TestControllerDoneClosesWhenTransportStops(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)

	close(transport.incoming)

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}

	_, ok := <-controller.Messages()
	require.False(t, ok)
}

func TestControllerDoneClosesOnTransportErrorAndContextCancel(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)
	transport.errs <- errors.New("transport failed")

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}

	ctx, cancel := context.WithCancel(context.Background())
	transport = newFakeTransport()
	controller = NewController(nil, transport)
	startControllerForTest(t, controller, ctx)
	cancel()

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}
}

func TestControllerStartStopsWhenDoneClosed(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.Start(context.Background())
	controller.stop()

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}
}

func TestControllerStartStopsWhenDataQueueOverflows(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	for range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "queued"}
	}

	controller.Start(context.Background())
	controller.dataIn <- map[string]any{"type": "queued"}
	require.Eventually(t, func() bool {
		return len(controller.dataIn) == 0
	}, time.Second, 10*time.Millisecond)

	for range cap(controller.dataIn) {
		controller.dataIn <- map[string]any{"type": "queued"}
	}

	transport.incoming <- map[string]any{"type": "assistant"}

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}
	require.True(t, transport.isClosed())
}

func TestControllerDrainsMessagesAfterTransportError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{"type": "assistant"}
	transport.errs <- errors.New("transport failed")
	close(transport.incoming)

	select {
	case msg := <-controller.Messages():
		require.Equal(t, "assistant", msg["type"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for drained data message")
	}

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}
}

func TestControllerDrainMessagesStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	controller := NewController(nil, newFakeTransport())
	controller.drainMessages(ctx, make(chan map[string]any))
}

func TestControllerDrainMessagesRoutesBufferedMessages(t *testing.T) {
	t.Parallel()

	messages := make(chan map[string]any, 1)
	messages <- map[string]any{"type": "assistant"}
	close(messages)

	controller := NewController(nil, newFakeTransport())
	go controller.pumpDataMessages()
	t.Cleanup(controller.closeDataPump)

	controller.drainMessages(context.Background(), messages)

	select {
	case msg := <-controller.Messages():
		require.Equal(t, "assistant", msg["type"])
	case <-time.After(time.Second):
		t.Fatal("buffered message was not drained")
	}
}

func TestControllerDrainMessagesStopsWhenRouteFails(t *testing.T) {
	t.Parallel()

	messages := make(chan map[string]any, 1)
	messages <- map[string]any{"type": "assistant"}
	close(messages)

	controller := NewController(nil, newFakeTransport())
	for range cap(controller.dataIn) {
		controller.dataIn <- map[string]any{"type": "queued"}
	}

	controller.drainMessages(context.Background(), messages)
}

func TestControllerCloseDataPumpStopsBlockedPump(t *testing.T) {
	t.Parallel()

	controller := NewController(nil, newFakeTransport())
	for range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "queued"}
	}

	go controller.pumpDataMessages()
	controller.dataIn <- map[string]any{"type": "assistant"}
	controller.closeDataPump()

	select {
	case <-controller.dataDone:
	default:
		t.Fatal("data pump did not stop")
	}
}

func TestControllerSendRequestReceivesResponse(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	ctx = startControllerForTest(t, controller, ctx)

	done := make(chan *ControlResponse, 1)
	errs := make(chan error, 1)
	go func() {
		resp, err := controller.SendRequest(ctx, "initialize", map[string]any{"x": true}, time.Second)
		if err != nil {
			errs <- err

			return
		}

		done <- resp
	}()

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	sent, ok := transport.sentPayloads()[0].(ControlRequest)
	require.True(t, ok)
	require.Equal(t, "initialize", sent.Request["subtype"])
	require.Equal(t, true, sent.Request["x"])

	transport.incoming <- map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": sent.RequestID,
			"response":   map[string]any{"ok": true},
		},
	}

	select {
	case err := <-errs:
		require.NoError(t, err)
	case resp := <-done:
		payload, ok := resp.Response["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, payload["ok"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control response")
	}
}

func TestControllerRoutesControlResponseWhileDataOutputBlocked(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	for range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "queued"}
	}

	ctx = startControllerForTest(t, controller, ctx)
	transport.incoming <- map[string]any{"type": "assistant", "message": "blocked"}

	done := make(chan *ControlResponse, 1)
	errs := make(chan error, 1)
	go func() {
		resp, err := controller.SendRequest(ctx, "initialize", nil, time.Second)
		if err != nil {
			errs <- err

			return
		}

		done <- resp
	}()

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	sent, ok := transport.sentPayloads()[0].(ControlRequest)
	require.True(t, ok)
	transport.incoming <- map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": sent.RequestID,
			"response":   map[string]any{"ok": true},
		},
	}

	select {
	case err := <-errs:
		require.NoError(t, err)
	case resp := <-done:
		payload, ok := resp.Response["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, payload["ok"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control response")
	}
}

func TestControllerRouteRequestRejectsWhenHandlerLimitReached(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	for range maxConcurrentControlHandlers {
		controller.handlerSem <- struct{}{}
	}

	controller.routeRequest(context.Background(), map[string]any{
		keyRequestID: "req_1",
	})

	sent := transport.sentPayloads()
	require.Len(t, sent, 1)

	resp, ok := sent[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, responseSubtypeError, resp.Response[keySubtype])
	require.Equal(t, "req_1", resp.Response[keyRequestID])

	transport.sendErr = errors.New("send failed")
	controller.routeRequest(context.Background(), map[string]any{
		keyRequestID: "req_2",
	})
}

func TestControllerRouteResponseDropsDuplicateWithoutBlocking(t *testing.T) {
	t.Parallel()

	controller := NewController(nil, newFakeTransport())
	ch := make(chan *ControlResponse, 1)
	controller.pending["req_1"] = ch
	msg := map[string]any{
		keyResponse: map[string]any{
			keyRequestID: "req_1",
		},
	}

	controller.routeResponse(msg)

	done := make(chan struct{})
	go func() {
		controller.routeResponse(msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("duplicate control response blocked routing")
	}
	require.Len(t, ch, 1)
}

func TestControllerRouteDropsDataMessageAfterContextOrStop(t *testing.T) {
	t.Parallel()

	controller := NewController(nil, newFakeTransport())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, controller.route(ctx, map[string]any{"type": "assistant"}))

	controller = NewController(nil, newFakeTransport())
	controller.stop()
	require.False(t, controller.route(context.Background(), map[string]any{"type": "assistant"}))

	controller = NewController(nil, newFakeTransport())
	close(controller.dataStop)
	require.False(t, controller.route(context.Background(), map[string]any{"type": "assistant"}))
}

func TestControllerRouteDataOverflowClosesTransport(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	for range cap(controller.dataIn) {
		controller.dataIn <- map[string]any{"type": "queued"}
	}

	require.False(t, controller.route(context.Background(), map[string]any{"type": "assistant"}))
	require.True(t, transport.isClosed())
}

func TestControllerSendRequestErrorResponse(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	ctx = startControllerForTest(t, controller, ctx)

	errs := make(chan error, 1)
	go func() {
		_, err := controller.SendRequest(ctx, "set_model", nil, time.Second)
		errs <- err
	}()

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	sent, ok := transport.sentPayloads()[0].(ControlRequest)
	require.True(t, ok)

	transport.incoming <- map[string]any{
		keyType: controlResponseType,
		keyResponse: map[string]any{
			keySubtype:           responseSubtypeError,
			keyRequestID:         sent.RequestID,
			responseSubtypeError: "bad model",
		},
	}

	err := <-errs
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad model")
}

func TestControlRequestErrorClassifiesSessionFailures(t *testing.T) {
	t.Parallel()

	err := controlRequestError("resume", "No conversation found with session ID abc")
	require.ErrorIs(t, err, ErrSessionNotFound)

	err = controlRequestError("query", "Query closed before response received")
	require.ErrorIs(t, err, ErrQueryClosed)
}

func TestControllerSendRequestTimeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	controller := NewController(nil, newFakeTransport())
	ctx = startControllerForTest(t, controller, ctx)

	_, err := controller.SendRequest(ctx, "initialize", nil, time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestControllerSendRequestFailures(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("send failed")
	transport := newFakeTransport()
	transport.sendErr = sendErr

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	controller := NewController(nil, transport)
	startControllerForTest(t, controller, runCtx)

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.ErrorIs(t, err, sendErr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport = newFakeTransport()
	runCtx, stop = context.WithCancel(context.Background())
	defer stop()

	controller = NewController(nil, transport)
	startControllerForTest(t, controller, runCtx)

	_, err = controller.SendRequest(ctx, "initialize", nil, time.Second)
	require.ErrorIs(t, err, context.Canceled)

	transport = newFakeTransport()
	controller = NewController(nil, transport)
	startControllerForTest(t, controller, context.Background())
	close(transport.incoming)

	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for controller done")
	}

	_, err = controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stopped")

	ctx, cancel = context.WithCancel(context.Background())
	cancel()

	transport = newFakeTransport()
	controller = NewController(nil, transport)
	startControllerForTest(t, controller, context.Background())

	_, err = controller.SendRequest(ctx, "initialize", nil, 0)
	require.ErrorIs(t, err, context.Canceled)
}

func TestControllerHandlesInboundRequest(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.RegisterHandler("can_use_tool", func(_ context.Context, req *ControlRequest) (map[string]any, error) {
		require.Equal(t, "req-1", req.RequestID)

		return map[string]any{"behavior": "allow"}, nil
	})
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{
		"type":       "control_request",
		"request_id": "req-1",
		"request": map[string]any{
			"subtype": "can_use_tool",
		},
	}

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, "success", resp.Response["subtype"])
	require.Equal(t, "req-1", resp.Response["request_id"])
}

func TestControllerHandlesInboundRequestErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.RegisterHandler("bad", func(context.Context, *ControlRequest) (map[string]any, error) {
		return nil, context.Canceled
	})
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "bad",
		},
	}
	transport.incoming <- map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-2",
		keyRequest: map[string]any{
			keySubtype: "missing",
		},
	}

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 2
	}, time.Second, 10*time.Millisecond)

	first, ok := transport.sentPayloads()[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, responseSubtypeError, first.Response[keySubtype])

	second, ok := transport.sentPayloads()[1].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, responseSubtypeError, second.Response[keySubtype])
}

func TestControllerRecoversInboundRequestHandlerPanic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.RegisterHandler("panic", func(context.Context, *ControlRequest) (map[string]any, error) {
		panic("boom")
	})
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "panic",
		},
	}

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, responseSubtypeError, resp.Response[keySubtype])
	require.Contains(t, resp.Response[responseSubtypeError], "handler panic: boom")
}

func TestControllerInboundRequestHandlerTimeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.SetHandlerTimeout(10 * time.Millisecond)

	started := make(chan struct{})
	release := make(chan struct{})
	controller.RegisterHandler("slow", func(context.Context, *ControlRequest) (map[string]any, error) {
		close(started)
		<-release

		return map[string]any{"late": true}, nil
	})
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "slow",
		},
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, responseSubtypeError, resp.Response[keySubtype])
	require.Contains(t, resp.Response[responseSubtypeError], "timed out")

	require.Eventually(t, func() bool {
		return len(controller.handlerSem) == 0
	}, time.Second, 10*time.Millisecond)

	close(release)
}

func TestControllerSetHandlerTimeoutDefault(t *testing.T) {
	t.Parallel()

	controller := NewController(nil, newFakeTransport())
	controller.SetHandlerTimeout(0)

	require.Equal(t, defaultControlHandlerTimeout, controller.handlerTimeout)
}

func TestControllerLogsControlResponseSendError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	transport.sendErr = errors.New("send failed")
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)

	transport.incoming <- map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "missing",
		},
	}

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)
}
