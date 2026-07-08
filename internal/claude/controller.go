package claude

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	controlRequestType  = "control_request"
	controlResponseType = "control_response"

	responseSubtypeError   = "error"
	responseSubtypeSuccess = "success"

	maxConcurrentControlHandlers = 64
	defaultControlHandlerTimeout = 5 * time.Minute
	controllerDataBuffer         = 1024
	controllerDataDrainTimeout   = 100 * time.Millisecond
)

type controlHandler func(context.Context, *ControlRequest) (map[string]any, error)

type controlHandlerResult struct {
	payload map[string]any
	err     error
}

// ControlRequest is a Claude control protocol request.
type ControlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"` //nolint:tagliatelle // Claude wire format.
	Request   map[string]any `json:"request"`
}

// ControlResponse is a Claude control protocol response.
type ControlResponse struct {
	Type     string         `json:"type"`
	Response map[string]any `json:"response"`
}

// Controller multiplexes Claude data messages and control messages.
type Controller struct {
	log       *slog.Logger
	transport Transport

	nextID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan *ControlResponse

	handlersMu     sync.RWMutex
	handlers       map[string]controlHandler
	handlerSem     chan struct{}
	handlerTimeout time.Duration

	dataIn   chan map[string]any
	dataStop chan struct{}
	dataDone chan struct{}
	messages chan map[string]any
	done     chan struct{}
	once     sync.Once

	lastErrMu sync.Mutex
	lastErr   error
}

// setLastError records the transport error that stopped routing so Receive can
// surface the real cause instead of a bare stream-closed sentinel.
func (c *Controller) setLastError(err error) {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()

	if c.lastErr == nil {
		c.lastErr = err
	}
}

// LastError returns the transport error that stopped routing, if any.
func (c *Controller) LastError() error {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()

	return c.lastErr
}

// NewController creates a control protocol controller.
func NewController(log *slog.Logger, transport Transport) *Controller {
	if log == nil {
		log = slog.Default()
	}

	return &Controller{
		log:            log,
		transport:      transport,
		pending:        make(map[string]chan *ControlResponse),
		handlers:       make(map[string]controlHandler),
		handlerSem:     make(chan struct{}, maxConcurrentControlHandlers),
		handlerTimeout: defaultControlHandlerTimeout,
		dataIn:         make(chan map[string]any, controllerDataBuffer),
		dataStop:       make(chan struct{}),
		dataDone:       make(chan struct{}),
		messages:       make(chan map[string]any, 64),
		done:           make(chan struct{}),
	}
}

// Start begins routing messages from the transport.
func (c *Controller) Start(ctx context.Context) {
	messages, errs := c.transport.Messages(ctx)

	go c.pumpDataMessages()

	go func() {
		defer c.recoverRouter(ctx)
		defer c.stop()
		defer c.closeDataPump()

		for {
			select {
			case msg, ok := <-messages:
				if !ok {
					return
				}

				if !c.route(ctx, msg) {
					return
				}
			case err, ok := <-errs:
				if ok && err != nil {
					c.setLastError(err)
					c.log.DebugContext(ctx, "claude transport error", slog.String(keyError, err.Error()))
				}

				c.drainMessages(ctx, messages)

				return
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
		}
	}()
}

// Done closes when routing stops.
func (c *Controller) Done() <-chan struct{} {
	return c.done
}

func (c *Controller) stop() {
	c.once.Do(func() {
		close(c.done)
	})
}

// RegisterHandler registers an incoming control-request handler. Handlers must
// honor the provided context; the controller can return a timeout response, but
// Go cannot force-stop a handler goroutine that ignores cancellation.
func (c *Controller) RegisterHandler(subtype string, handler controlHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()

	c.handlers[subtype] = handler
}

// SetHandlerTimeout bounds one inbound control-request handler invocation.
func (c *Controller) SetHandlerTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultControlHandlerTimeout
	}

	c.handlerTimeout = timeout
}

// Messages returns non-control Claude messages.
func (c *Controller) Messages() <-chan map[string]any {
	return c.messages
}

// SendRequest sends a control request and waits for a correlated response.
func (c *Controller) SendRequest(
	ctx context.Context,
	subtype string,
	payload map[string]any,
	timeout time.Duration,
) (*ControlResponse, error) {
	id := fmt.Sprintf("req_%d", c.nextID.Add(1))

	if payload == nil {
		payload = make(map[string]any)
	}

	reqPayload := make(map[string]any, len(payload)+1)
	reqPayload[keySubtype] = subtype
	maps.Copy(reqPayload, payload)

	ch := make(chan *ControlResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.transport.Send(ctx, ControlRequest{
		Type:      controlRequestType,
		RequestID: id,
		Request:   reqPayload,
	}); err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = time.Minute
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		if respSubtype, _ := resp.Response[keySubtype].(string); respSubtype == responseSubtypeError {
			msg, _ := resp.Response[responseSubtypeError].(string)

			return nil, controlRequestError(subtype, msg)
		}

		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("claude control request %q timed out", subtype)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("claude control controller stopped")
	}
}

func controlRequestError(subtype string, msg string) error {
	err := fmt.Errorf("claude control request %q failed: %s", subtype, msg)
	switch {
	case strings.Contains(msg, "No conversation found with session ID"):
		return fmt.Errorf("%w: %w", ErrSessionNotFound, err)
	case strings.Contains(msg, "Query closed before response received"):
		return fmt.Errorf("%w: %w", ErrQueryClosed, err)
	default:
		return err
	}
}

func (c *Controller) drainMessages(ctx context.Context, messages <-chan map[string]any) {
	for {
		select {
		case msg, ok := <-messages:
			if !ok {
				return
			}

			if !c.route(ctx, msg) {
				return
			}
		case <-ctx.Done():
			return
		default:
			return
		}
	}
}

func (c *Controller) route(ctx context.Context, msg map[string]any) bool {
	msgType, _ := msg[keyType].(string)
	switch msgType {
	case controlResponseType:
		c.routeResponse(msg)

		return true
	case controlRequestType:
		c.routeRequest(ctx, msg)

		return true
	default:
		return c.routeData(ctx, msg)
	}
}

func (c *Controller) routeData(ctx context.Context, msg map[string]any) bool {
	select {
	case <-ctx.Done():
		return false
	case <-c.done:
		return false
	case <-c.dataStop:
		return false
	default:
	}

	select {
	case c.dataIn <- msg:
		return true
	default:
		c.log.DebugContext(ctx, "claude data message queue full")
		_ = c.transport.Close()

		return false
	}
}

func (c *Controller) closeDataPump() {
	close(c.dataIn)

	timer := time.NewTimer(controllerDataDrainTimeout)
	defer timer.Stop()

	select {
	case <-c.dataDone:
		return
	case <-timer.C:
		close(c.dataStop)
		<-c.dataDone
	}
}

func (c *Controller) pumpDataMessages() {
	defer close(c.messages)
	defer close(c.dataDone)

	for msg := range c.dataIn {
		select {
		case c.messages <- msg:
		case <-c.dataStop:
			return
		}
	}
}

func (c *Controller) routeRequest(ctx context.Context, msg map[string]any) {
	select {
	case c.handlerSem <- struct{}{}:
		go func() {
			defer c.recoverControlRequest(ctx)
			defer func() { <-c.handlerSem }()

			c.handleRequest(ctx, msg)
		}()
	default:
		c.rejectRequest(ctx, msg, "too many in-flight Claude control requests")
	}
}

func (c *Controller) recoverRouter(ctx context.Context) {
	handleClaudeGoroutinePanic(ctx, c.log, "controller router", func(any) {
		_ = c.transport.Close()
	}, recover())
}

func (c *Controller) recoverControlRequest(ctx context.Context) {
	handleClaudeGoroutinePanic(ctx, c.log, "control request handler", func(any) {
		c.stop()
		_ = c.transport.Close()
	}, recover())
}

func (c *Controller) routeResponse(msg map[string]any) {
	response, _ := msg[keyResponse].(map[string]any)
	requestID, _ := response[keyRequestID].(string)

	c.pendingMu.Lock()
	ch := c.pending[requestID]
	c.pendingMu.Unlock()

	if ch != nil {
		select {
		case ch <- &ControlResponse{Type: controlResponseType, Response: response}:
		default:
			c.log.Debug("drop duplicate Claude control response", slog.String(keyRequestID, requestID))
		}
	}
}

func (c *Controller) rejectRequest(ctx context.Context, msg map[string]any, message string) {
	requestID, _ := msg[keyRequestID].(string)
	response := map[string]any{
		keyRequestID:         requestID,
		keySubtype:           responseSubtypeError,
		responseSubtypeError: message,
	}

	if err := c.transport.Send(ctx, ControlResponse{Type: controlResponseType, Response: response}); err != nil {
		c.log.DebugContext(ctx, "send control response failed", slog.String(keyError, err.Error()))
	}
}

func (c *Controller) handleRequest(ctx context.Context, msg map[string]any) {
	request, _ := msg[keyRequest].(map[string]any)
	requestID, _ := msg[keyRequestID].(string)
	subtype, _ := request[keySubtype].(string)

	c.handlersMu.RLock()
	handler := c.handlers[subtype]
	c.handlersMu.RUnlock()

	var (
		payload map[string]any
		err     error
	)

	if handler == nil {
		err = fmt.Errorf("no handler registered for %q", subtype)
	} else {
		req := &ControlRequest{
			Type:      controlRequestType,
			RequestID: requestID,
			Request:   request,
		}
		payload, err = c.runHandler(ctx, subtype, handler, req)
	}

	response := map[string]any{keyRequestID: requestID}
	if err != nil {
		response[keySubtype] = responseSubtypeError
		response[responseSubtypeError] = err.Error()
	} else {
		response[keySubtype] = responseSubtypeSuccess
		response[keyResponse] = payload
	}

	sendErr := c.transport.Send(ctx, ControlResponse{Type: controlResponseType, Response: response})
	if sendErr != nil {
		c.log.DebugContext(ctx, "send control response failed", slog.String(keyError, sendErr.Error()))
	}
}

func (c *Controller) runHandler(
	ctx context.Context,
	subtype string,
	handler controlHandler,
	req *ControlRequest,
) (map[string]any, error) {
	handlerCtx, cancel := context.WithTimeout(ctx, c.handlerTimeout)
	defer cancel()

	resultCh := make(chan controlHandlerResult, 1)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				resultCh <- controlHandlerResult{err: fmt.Errorf("handler panic: %v", recovered)}
			}
		}()

		payload, err := handler(handlerCtx, req)
		resultCh <- controlHandlerResult{payload: payload, err: err}
	}()

	// Timing out frees the controller semaphore and returns a response; a handler
	// that ignores handlerCtx may still keep its own goroutine alive.
	select {
	case result := <-resultCh:
		return result.payload, result.err
	case <-handlerCtx.Done():
		c.log.DebugContext(
			ctx,
			"claude control request handler timed out or canceled",
			slog.String(keySubtype, subtype),
			slog.String(keyError, handlerCtx.Err().Error()),
		)

		return nil, fmt.Errorf("claude control request handler %q timed out or canceled: %w", subtype, handlerCtx.Err())
	}
}
