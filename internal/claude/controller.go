package claude

import (
	"context"
	"errors"
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
)

type controlHandler func(context.Context, *ControlRequest) (map[string]any, error)

// registeredControlHandler is one inbound control-request handler and whether
// its completion waits on the ACP client. A host-bound handler holds a
// permission or elicitation request open until the client answers, and the
// client owns how long that takes: it ends with the session's cancellation and
// teardown, never with the wall-clock handler timeout.
type registeredControlHandler struct {
	handle    controlHandler
	hostBound bool
}

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
	handlers       map[string]registeredControlHandler
	handlerSem     chan struct{}
	handlerWG      sync.WaitGroup
	handlerTimeout time.Duration
	fatal          chan error

	dataIn    chan map[string]any
	dataSlots chan struct{}
	dataAbort chan struct{}
	abortOnce sync.Once
	dataDone  chan struct{}
	messages  chan map[string]any
	done      chan struct{}
	once      sync.Once

	lastErrMu sync.Mutex
	lastErr   error
}

func (c *Controller) joinLastError(err error) {
	closed := closedTransportError(err)
	if closed == nil {
		return
	}

	c.lastErrMu.Lock()
	c.lastErr = errors.Join(c.lastErr, closed)
	c.lastErrMu.Unlock()
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
		handlers:       make(map[string]registeredControlHandler),
		handlerSem:     make(chan struct{}, maxConcurrentControlHandlers),
		handlerTimeout: defaultControlHandlerTimeout,
		fatal:          make(chan error, 1),
		dataIn:         make(chan map[string]any, controllerDataBuffer),
		dataSlots:      make(chan struct{}, controllerDataBuffer),
		dataAbort:      make(chan struct{}),
		dataDone:       make(chan struct{}),
		messages:       make(chan map[string]any, 64),
		done:           make(chan struct{}),
	}
}

// Start begins routing messages from the transport.
func (c *Controller) Start(ctx context.Context) {
	producerCtx, cancelProducer := context.WithCancel(ctx)
	events := c.transport.Events(producerCtx)

	go c.pumpDataMessages()

	go func() {
		var terminal error

		abortData := false
		stopProducer := false

		defer func() {
			if recovered := recover(); recovered != nil {
				terminal = errors.Join(terminal, errClaudeTransportFailure)
				abortData = true
				stopProducer = true

				handleClaudeGoroutinePanic(ctx, c.log, "controller router", nil, recovered)
			}

			cancelProducer()

			if stopProducer {
				closed := make(chan error, 1)
				go func() { closed <- c.transport.Close() }()

				for range events {
				}

				terminal = errors.Join(terminal, <-closed)
			}

			c.handlerWG.Wait()

			if abortData {
				c.AbortData()
			}
			// Publish the terminal classification before the data channel can close,
			// so a receiver observing ordered drain completion also observes the
			// exact cause for this generation.
			c.joinLastError(terminal)
			close(c.dataIn)
			<-c.dataDone
			c.once.Do(func() { close(c.done) })
		}()

		for {
			select {
			case event, ok := <-events:
				if !ok {
					if cause := context.Cause(ctx); cause != nil {
						terminal = errors.Join(terminal, cause,
							&ControllerDataError{Kind: ControllerDataTeardownAbort})
						abortData = true
						stopProducer = true
					}

					return
				}

				if event.Err != nil {
					terminal = event.Err
					stopProducer = true

					c.log.DebugContext(ctx, "claude transport error",
						slog.String("class", transportErrorClass(event.Err)))

					return
				}

				if err := c.route(producerCtx, event.Message); err != nil {
					terminal = err
					stopProducer = true

					return
				}
			case fatal := <-c.fatal:
				terminal = errors.Join(terminal, fatal)
				stopProducer = true

				return
			}
		}
	}()
}

// AbortData interrupts delivery of the admitted data prefix when its consumer
// cannot complete the shutdown boundary.
func (c *Controller) AbortData() {
	c.abortOnce.Do(func() { close(c.dataAbort) })
}

// Done closes when routing stops.
func (c *Controller) Done() <-chan struct{} {
	return c.done
}

func (c *Controller) submitFatal(cause error) {
	select {
	case c.fatal <- cause:
	default:
	}
}

// RegisterHandler registers an incoming control-request handler the adapter
// answers on its own, bounded by the handler timeout. Handlers must honor the
// provided context; the controller can return a timeout response, but Go cannot
// force-stop a handler goroutine that ignores cancellation.
func (c *Controller) RegisterHandler(subtype string, handler controlHandler) {
	c.registerHandler(subtype, registeredControlHandler{handle: handler})
}

// RegisterHostBoundHandler registers an incoming control-request handler whose
// answer comes from the ACP client — a permission decision or an elicitation
// response. The handler timeout does not apply: a question stays open for as
// long as the client keeps it open, and ends with the session's cancellation or
// teardown context instead.
func (c *Controller) RegisterHostBoundHandler(subtype string, handler controlHandler) {
	c.registerHandler(subtype, registeredControlHandler{handle: handler, hostBound: true})
}

func (c *Controller) registerHandler(subtype string, handler registeredControlHandler) {
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
		return nil, closedTransportError(err)
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
		return nil, controllerStoppedError(c.LastError())
	}
}

// controllerStoppedError reports a control request that outlived its
// controller. The bare stop says only that routing ended, so the transport
// cause is attached only after the router has reduced it to a closed status or
// stage classification.
func controllerStoppedError(cause error) error {
	if cause == nil {
		return errors.New("claude control controller stopped")
	}

	return fmt.Errorf("claude control controller stopped: %w", cause)
}

func controlRequestError(subtype string, msg string) error {
	switch {
	case strings.Contains(msg, "No conversation found with session ID"):
		return errors.Join(ErrSessionNotFound, errClaudeControlRequestFail)
	case strings.Contains(msg, "Query closed before response received"):
		return errors.Join(ErrQueryClosed, errClaudeControlRequestFail)
	default:
		return errClaudeControlRequestFail
	}
}

func (c *Controller) route(ctx context.Context, msg map[string]any) error {
	msgType, _ := msg[keyType].(string)
	switch msgType {
	case controlResponseType:
		c.routeResponse(msg)

		return nil
	case controlRequestType:
		c.routeRequest(ctx, msg)

		return nil
	default:
		return c.routeData(ctx, msg)
	}
}

func (c *Controller) routeData(ctx context.Context, msg map[string]any) error {
	select {
	case <-ctx.Done():
		return errors.Join(context.Cause(ctx), &ControllerDataError{Kind: ControllerDataTeardownAbort})
	default:
	}

	select {
	case c.dataSlots <- struct{}{}:
	default:
		c.log.DebugContext(ctx, "claude data message queue full")

		return &ControllerDataError{Kind: ControllerDataOverflow}
	}

	// The router is the sole producer, and a slot remains held until the frame is
	// delivered to the consumer. The bounded admission count therefore includes
	// the pump's in-flight frame and cannot vary with scheduler timing.
	c.dataIn <- msg

	return nil
}

func (c *Controller) pumpDataMessages() {
	defer close(c.messages)
	defer close(c.dataDone)

	for msg := range c.dataIn {
		select {
		case c.messages <- msg:
			<-c.dataSlots
		case <-c.dataAbort:
			return
		}
	}
}

func (c *Controller) routeRequest(ctx context.Context, msg map[string]any) {
	select {
	case c.handlerSem <- struct{}{}:
		c.handlerWG.Add(1)
		go func() {
			defer c.handlerWG.Done()
			defer func() { <-c.handlerSem }()

			c.handleRequest(ctx, msg)
		}()
	default:
		c.rejectRequest(ctx, msg, "too many in-flight Claude control requests")
	}
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

	if err := c.sendControlResponse(ctx, response); err != nil {
		c.log.DebugContext(ctx, "send control response failed", slog.String("class", transportErrorClass(err)))
		c.submitFatal(err)
	}
}

func (c *Controller) handleRequest(ctx context.Context, msg map[string]any) {
	request, _ := msg[keyRequest].(map[string]any)
	requestID, _ := msg[keyRequestID].(string)
	subtype, _ := request[keySubtype].(string)

	c.handlersMu.RLock()
	handler, registered := c.handlers[subtype]
	c.handlersMu.RUnlock()

	var (
		payload map[string]any
		err     error
		wait    = func() {}
	)

	if !registered {
		err = fmt.Errorf("no handler registered for %q", subtype)
	} else {
		req := &ControlRequest{
			Type:      controlRequestType,
			RequestID: requestID,
			Request:   request,
		}
		payload, wait, err = c.runHandler(ctx, subtype, handler, req)
	}

	response := map[string]any{keyRequestID: requestID}
	if err != nil {
		response[keySubtype] = responseSubtypeError
		response[responseSubtypeError] = controlHandlerWireError(err)
	} else {
		response[keySubtype] = responseSubtypeSuccess
		response[keyResponse] = payload
	}

	sendErr := c.sendControlResponse(ctx, response)
	if sendErr != nil {
		c.log.DebugContext(ctx, "send control response failed", slog.String("class", transportErrorClass(sendErr)))
		c.submitFatal(sendErr)
	}

	wait()
}

func (c *Controller) sendControlResponse(ctx context.Context, response map[string]any) (err error) {
	defer func() {
		if recover() != nil {
			err = errControlResponseWrite
		}
	}()

	if err := c.transport.Send(ctx, ControlResponse{Type: controlResponseType, Response: response}); err != nil {
		return errors.Join(errControlResponseWrite, closedTransportError(err))
	}

	return nil
}

func (c *Controller) runHandler(
	ctx context.Context,
	subtype string,
	handler registeredControlHandler,
	req *ControlRequest,
) (map[string]any, func(), error) {
	handlerCtx, cancel := c.handlerContext(ctx, handler)
	defer cancel()

	resultCh := make(chan controlHandlerResult, 1)

	workerDone := make(chan struct{})

	go func() {
		defer close(workerDone)
		defer func() {
			if recover() != nil {
				c.submitFatal(errControlHandlerPanic)

				resultCh <- controlHandlerResult{err: errControlHandlerPanic}
			}
		}()

		payload, err := handler.handle(handlerCtx, req)
		resultCh <- controlHandlerResult{payload: payload, err: err}
	}()

	// The timeout fixes the wire response time. The caller retains the controller
	// permit until workerDone, so a handler that ignores cancellation still counts
	// against the true worker bound.
	select {
	case result := <-resultCh:
		if result.err != nil {
			return nil, func() {}, closedControlHandlerError(result.err)
		}

		return result.payload, func() {}, nil
	case <-handlerCtx.Done():
		c.log.DebugContext(
			ctx,
			"claude control request handler timed out or canceled",
			slog.String("stage", "handler_wait"),
		)

		return nil, func() { <-workerDone }, closedControlHandlerError(handlerCtx.Err())
	}
}

// handlerContext bounds one handler invocation: the wall-clock handler timeout
// for a request the adapter answers itself, only the routing context for one
// that waits on the ACP client.
func (c *Controller) handlerContext(
	ctx context.Context,
	handler registeredControlHandler,
) (context.Context, context.CancelFunc) {
	if handler.hostBound {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, c.handlerTimeout)
}

func closedControlHandlerError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, errControlHandlerPanic):
		return errControlHandlerPanic
	default:
		return errControlHandlerFailure
	}
}

func controlHandlerWireError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "control request handler canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "control request handler timed out"
	case errors.Is(err, errControlHandlerPanic):
		return errControlHandlerPanic.Error()
	default:
		return errControlHandlerFailure.Error()
	}
}
