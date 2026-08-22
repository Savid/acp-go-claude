package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeTransport struct {
	events chan TransportEvent

	mu   sync.Mutex
	sent []any

	sendErr   error
	panicSend bool
	closeErr  error
	closed    bool
	closes    int
	closeSig  chan struct{}
	closeOne  sync.Once
}

type panicOnceHandler struct {
	panicked bool
}

func (h *panicOnceHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *panicOnceHandler) Handle(context.Context, slog.Record) error {
	if !h.panicked {
		h.panicked = true

		panic("observer panic")
	}

	return nil
}

func (h *panicOnceHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *panicOnceHandler) WithGroup(string) slog.Handler { return h }

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		events:   make(chan TransportEvent, 16),
		closeSig: make(chan struct{}),
	}
}

func (f *fakeTransport) Start(context.Context) error { return nil }

func (f *fakeTransport) Send(_ context.Context, payload any) error {
	f.mu.Lock()
	if f.panicSend {
		f.mu.Unlock()

		panic("send panic")
	}
	f.sent = append(f.sent, payload)
	err := f.sendErr
	f.mu.Unlock()

	return err
}

func (f *fakeTransport) Events(ctx context.Context) <-chan TransportEvent {
	events := make(chan TransportEvent)

	go func() {
		defer close(events)

		for {
			select {
			case event, ok := <-f.events:
				if !ok {
					return
				}

				select {
				case events <- event:
				case <-ctx.Done():
					return
				case <-f.closeSig:
					return
				}
			case <-ctx.Done():
				return
			case <-f.closeSig:
				return
			}
		}
	}()

	return events
}

func (f *fakeTransport) sendMessage(message map[string]any) {
	f.events <- TransportEvent{Message: message}
}

func (f *fakeTransport) sendError(err error) {
	f.events <- TransportEvent{Err: err}
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true
	f.closes++
	f.closeOne.Do(func() { close(f.closeSig) })

	return f.closeErr
}

func (f *fakeTransport) closeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closes
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

func TestControllerRouterOwnsDoneAfterControlResponsePanicAndDataJoin(t *testing.T) {
	transport := newFakeTransport()
	controller := NewController(slog.New(slog.DiscardHandler), transport)
	for index := range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "blocked", "number": index}
	}
	startControllerForTest(t, controller, t.Context())

	transport.sendMessage(map[string]any{"type": "assistant", "number": cap(controller.messages)})
	transport.mu.Lock()
	transport.panicSend = true
	transport.mu.Unlock()
	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "panic-response",
		keyRequest:   map[string]any{keySubtype: "missing"},
	})

	<-transport.closeSig

	select {
	case <-controller.Done():
		t.Fatal("the router published Done before joining its blocked data prefix")
	default:
	}

	for range cap(controller.messages) + 1 {
		<-controller.Messages()
	}
	<-controller.Done()
	require.ErrorIs(t, controller.LastError(), errControlResponseWrite)
}

func TestControllerRoutesDataMessages(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	startControllerForTest(t, controller, ctx)

	transport.sendMessage(map[string]any{"type": "assistant"})

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

	close(transport.events)

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
	transport.sendError(errors.New("transport failed"))
	close(transport.events)

	<-controller.Done()

	ctx, cancel := context.WithCancel(context.Background())
	transport = newFakeTransport()
	controller = NewController(nil, transport)
	startControllerForTest(t, controller, ctx)
	cancel()

	<-controller.Done()

	var failure *ControllerDataError
	require.ErrorAs(t, controller.LastError(), &failure)
	require.Equal(t, ControllerDataTeardownAbort, failure.Kind)
	require.ErrorIs(t, controller.LastError(), context.Canceled)
}

func TestControllerContainsObserverPanicAtTransportFailureBoundary(t *testing.T) {
	transport := newFakeTransport()
	controller := NewController(slog.New(&panicOnceHandler{}), transport)
	startControllerForTest(t, controller, t.Context())

	transport.sendError(errors.New("transport failed"))
	<-controller.Done()

	require.ErrorIs(t, controller.LastError(), errClaudeTransportFailure)
	require.True(t, transport.isClosed())
}

func TestControllerEOFDrainsEveryAdmittedFrameInOrder(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	transport.events = make(chan TransportEvent, 8)
	controller := NewController(nil, transport)
	for i := range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "blocked", "number": i}
	}
	controller.Start(t.Context())

	for i := range 8 {
		transport.sendMessage(map[string]any{"type": "assistant", "number": i})
	}
	close(transport.events)

	// Keep the consumer gated beyond the abandoned implementation's 100 ms
	// cutoff. EOF must remain in its ordered drain state rather than dropping a
	// suffix to make Done close.
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	select {
	case <-controller.Done():
		t.Fatal("controller completed before the admitted EOF suffix was consumed")
	default:
	}

	for i := range cap(controller.messages) {
		msg := <-controller.Messages()
		require.Equal(t, "blocked", msg["type"])
		require.Equal(t, i, msg["number"])
	}
	for i := range 8 {
		msg := <-controller.Messages()
		require.Equal(t, "assistant", msg["type"])
		require.Equal(t, i, msg["number"])
	}

	_, open := <-controller.Messages()
	require.False(t, open)
	<-controller.Done()
	require.NoError(t, controller.LastError())
}

func TestControllerTerminalEventDrainsItsCausalPrefix(t *testing.T) {
	transport := newFakeTransport()
	transport.events = make(chan TransportEvent, 9)
	controller := NewController(nil, transport)
	for i := range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "blocked", "number": i}
	}
	controller.Start(t.Context())

	for i := range 8 {
		transport.sendMessage(map[string]any{"type": "assistant", "number": i})
	}
	terminal := errors.New("ordered terminal")
	transport.sendError(terminal)
	close(transport.events)

	for i := range cap(controller.messages) {
		msg := <-controller.Messages()
		require.Equal(t, "blocked", msg["type"])
		require.Equal(t, i, msg["number"])
	}
	for i := range 8 {
		msg := <-controller.Messages()
		require.Equal(t, "assistant", msg["type"])
		require.Equal(t, i, msg["number"])
	}

	_, open := <-controller.Messages()
	require.False(t, open)
	<-controller.Done()
	require.ErrorIs(t, controller.LastError(), errClaudeTransportFailure)
	require.NotContains(t, controller.LastError().Error(), terminal.Error())
}

func TestControllerOverflowDrainsAdmittedPrefixAndReportsTypedCause(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	transport.events = make(chan TransportEvent, controllerDataBuffer+2)
	controller := NewController(nil, transport)
	for i := range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "blocked", "number": i}
	}
	controller.Start(t.Context())

	for i := range controllerDataBuffer + 2 {
		transport.sendMessage(map[string]any{"type": "assistant", "number": i})
	}
	close(transport.events)
	<-transport.closeSig

	for i := range cap(controller.messages) {
		msg := <-controller.Messages()
		require.Equal(t, "blocked", msg["type"])
		require.Equal(t, i, msg["number"])
	}

	number := 0
	for msg := range controller.Messages() {
		require.Equal(t, "assistant", msg["type"])
		require.Equal(t, number, msg["number"])
		number++
	}
	require.Equal(t, controllerDataBuffer, number)
	<-controller.Done()
	require.True(t, transport.isClosed())

	var failure *ControllerDataError
	require.ErrorAs(t, controller.LastError(), &failure)
	require.Equal(t, ControllerDataOverflow, failure.Kind)
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

	transport.sendMessage(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": sent.RequestID,
			"response":   map[string]any{"ok": true},
		},
	})

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
	transport.sendMessage(map[string]any{"type": "assistant", "message": "blocked"})

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
	transport.sendMessage(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": sent.RequestID,
			"response":   map[string]any{"ok": true},
		},
	})

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
	err := controller.route(ctx, map[string]any{"type": "assistant"})
	require.ErrorIs(t, err, context.Canceled)

	var failure *ControllerDataError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, ControllerDataTeardownAbort, failure.Kind)
}

func TestControllerRouteDataOverflowReturnsTypedCause(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	controller := NewController(nil, transport)
	for range cap(controller.dataSlots) {
		controller.dataSlots <- struct{}{}
	}

	err := controller.route(context.Background(), map[string]any{"type": "assistant"})
	var failure *ControllerDataError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, ControllerDataOverflow, failure.Kind)
	require.False(t, transport.isClosed(), "the router owns teardown after classifying the cause")
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

	transport.sendMessage(map[string]any{
		keyType: controlResponseType,
		keyResponse: map[string]any{
			keySubtype:           responseSubtypeError,
			keyRequestID:         sent.RequestID,
			responseSubtypeError: "bad model",
		},
	})

	err := <-errs
	require.ErrorIs(t, err, errClaudeControlRequestFail)
	require.NotContains(t, err.Error(), "bad model")
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

	sendErr := errors.New("opaque-send-failure")
	transport := newFakeTransport()
	transport.sendErr = sendErr

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()

	controller := NewController(nil, transport)
	startControllerForTest(t, controller, runCtx)

	_, err := controller.SendRequest(context.Background(), "initialize", nil, time.Second)
	require.ErrorIs(t, err, errClaudeTransportFailure)
	require.NotContains(t, err.Error(), sendErr.Error())

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
	close(transport.events)

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

	transport.sendMessage(map[string]any{
		"type":       "control_request",
		"request_id": "req-1",
		"request": map[string]any{
			"subtype": "can_use_tool",
		},
	})

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

	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "bad",
		},
	})
	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-2",
		keyRequest: map[string]any{
			keySubtype: "missing",
		},
	})

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

	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "panic",
		},
	})

	<-controller.Done()
	require.True(t, transport.isClosed())
	require.ErrorIs(t, controller.LastError(), errControlHandlerPanic)
	for _, payload := range transport.sentPayloads() {
		resp, ok := payload.(ControlResponse)
		if ok {
			require.NotContains(t, resp.Response[responseSubtypeError], "boom")
		}
	}
}

func TestControllerClosesOpaqueHandlerErrorOnWireAndLogs(t *testing.T) {
	t.Parallel()

	const sentinel = "provider-store-tool-user-secret"
	var logs bytes.Buffer
	transport := newFakeTransport()
	controller := NewController(
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		transport,
	)
	controller.RegisterHandler("sensitive", func(context.Context, *ControlRequest) (map[string]any, error) {
		return nil, errors.New(sentinel)
	})

	controller.handleRequest(t.Context(), map[string]any{
		keyRequestID: "req-sensitive",
		keyRequest:   map[string]any{keySubtype: "sensitive"},
	})

	require.Len(t, transport.sentPayloads(), 1)
	response, ok := transport.sentPayloads()[0].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, errControlHandlerFailure.Error(), response.Response[responseSubtypeError])
	require.NotContains(t, response.Response[responseSubtypeError], sentinel)
	require.NotContains(t, logs.String(), sentinel)
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

	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "slow",
		},
	})

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

	require.Len(t, controller.handlerSem, 1)

	close(release)
	require.Eventually(t, func() bool {
		return len(controller.handlerSem) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestControllerTimeoutKeepsTrueWorkersBoundedAtSixtyFour(t *testing.T) {
	transport := newFakeTransport()
	controller := NewController(nil, transport)
	controller.SetHandlerTimeout(5 * time.Millisecond)

	started := make(chan struct{}, maxConcurrentControlHandlers)
	release := make(chan struct{})
	controller.RegisterHandler("stubborn", func(context.Context, *ControlRequest) (map[string]any, error) {
		started <- struct{}{}
		<-release

		return map[string]any{}, nil
	})

	for index := range maxConcurrentControlHandlers + 1 {
		controller.routeRequest(t.Context(), map[string]any{
			keyRequestID: fmt.Sprintf("req-%d", index),
			keyRequest: map[string]any{
				keySubtype: "stubborn",
			},
		})
	}

	require.Eventually(t, func() bool {
		return len(started) == maxConcurrentControlHandlers &&
			len(controller.handlerSem) == maxConcurrentControlHandlers &&
			len(transport.sentPayloads()) == maxConcurrentControlHandlers+1
	}, time.Second, time.Millisecond)

	close(release)
	require.Eventually(t, func() bool {
		return len(controller.handlerSem) == 0
	}, time.Second, time.Millisecond)
}

func TestControllerSetHandlerTimeoutDefault(t *testing.T) {
	t.Parallel()

	controller := NewController(nil, newFakeTransport())
	controller.SetHandlerTimeout(0)

	require.Equal(t, defaultControlHandlerTimeout, controller.handlerTimeout)
}

func TestControllerLogsControlResponseSendError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	const sentinel = "opaque-response-write-failure"
	transport := newFakeTransport()
	transport.sendErr = errors.New(sentinel)
	controller := NewController(
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		transport,
	)

	controller.handleRequest(t.Context(), map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "req-1",
		keyRequest: map[string]any{
			keySubtype: "missing",
		},
	})

	require.Len(t, transport.sentPayloads(), 1)
	require.NotContains(t, logs.String(), sentinel)
}
