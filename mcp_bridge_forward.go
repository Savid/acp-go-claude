package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/observer"
)

func (c *mcpBridgeConn) forwardProxyMessageAsync(ctx context.Context, msg mcpRPCMessage) {
	forwards, started, closed := c.tryStartForward()
	if closed {
		return
	}

	if started {
		go func() {
			defer c.recoverForward(ctx, "MCP proxy forward")
			defer c.wg.Done()
			defer func() { <-forwards }()

			c.forwardProxyMessage(ctx, msg)
		}()

		return
	}

	// Backpressure is explicit: reject overflow requests in-band instead of
	// blocking the proxy reader and deadlocking unrelated responses.
	if msg.ID != nil {
		c.logMCPProxySendFailure(
			ctx,
			c.sendMCPError(*msg.ID, -32000, "too many in-flight MCP proxy messages", nil),
		)
	}
}

func (c *mcpBridgeConn) recoverForward(ctx context.Context, name string) {
	handleAgentGoroutinePanic(ctx, c.agent.log, name, func(any) {
		c.Close()
	}, recover())
}

func (c *mcpBridgeConn) requestTimeout() time.Duration {
	if c.timeout <= 0 {
		return mcpProxyTimeout
	}

	return c.timeout
}

func (c *mcpBridgeConn) tryStartForward() (chan struct{}, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing {
		return nil, false, true
	}

	if c.forwards == nil {
		c.forwards = make(chan struct{}, mcpMaxForwards)
	}

	select {
	case c.forwards <- struct{}{}:
		c.wg.Add(1)

		return c.forwards, true, false
	default:
		return c.forwards, false, false
	}
}

func (c *mcpBridgeConn) handleProxyResponse(msg mcpRPCMessage) {
	id := mcpIDKey(msg.ID)

	c.mu.Lock()

	ch := c.pending[id]
	if ch != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ch != nil {
		ch <- msg
	}
}

func (c *mcpBridgeConn) forwardProxyMessage(ctx context.Context, msg mcpRPCMessage) {
	start := time.Now()

	metricResult := observer.MCPMessageResult{
		Direction:       "proxy_to_acp",
		Kind:            mcpMessageKind(msg),
		Method:          msg.Method,
		ProtocolVersion: mcpProtocolVersionFromRawParams(msg.Params),
		Transport:       mcpConfigTypeACP,
	}

	ctx, finishSpan := c.agent.observe.StartMCPBridge(ctx, "forward", metricResult)
	defer func() {
		finishSpan(metricResult.Err)
		c.agent.observe.RecordMCPBridgeMessage(ctx, start, metricResult)
	}()

	params, err := mcpParamsMap(msg.Params)
	if err != nil {
		metricResult.Err = err

		if msg.ID != nil {
			c.logMCPProxySendFailure(ctx, c.sendMCPError(*msg.ID, -32602, err.Error(), nil))
		}

		return
	}

	acpConn := c.agent.connection()
	if acpConn == nil {
		metricResult.Err = errACPConnectionNotAttached

		if msg.ID != nil {
			c.logMCPProxySendFailure(ctx, c.sendMCPError(*msg.ID, -32603, "ACP client is unavailable", nil))
		}

		return
	}

	if msg.ID == nil {
		err = acpConn.UnstableNotifyMcp(ctx, acp.UnstableMessageMcpNotification{
			ConnectionId: c.connectionID,
			Method:       msg.Method,
			Params:       params,
		})
		if err != nil {
			metricResult.Err = err
			c.agent.log.DebugContext(ctx, "forward MCP notification to ACP failed", slog.String(jsonFieldError, err.Error()))
		}

		return
	}

	result, err := acpConn.UnstableMessageMcp(ctx, acp.UnstableMessageMcpRequest{
		ConnectionId: c.connectionID,
		Method:       msg.Method,
		Params:       params,
	})
	if err != nil {
		metricResult.Err = err
		reqErr := requestError(err)
		c.logMCPProxySendFailure(ctx, c.sendMCPError(*msg.ID, reqErr.Code, reqErr.Message, reqErr.Data))

		return
	}

	c.logMCPProxySendFailure(ctx, c.sendMCPResult(*msg.ID, result))
}

func (c *mcpBridgeConn) logMCPProxySendFailure(ctx context.Context, err error) {
	if err != nil {
		c.agent.log.DebugContext(ctx, "send MCP proxy response failed", slog.String(jsonFieldError, err.Error()))
	}
}

func (c *mcpBridgeConn) sendMCPResult(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}

	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = nil
	}

	return c.send(mcpRPCMessage{ID: &id, Result: raw})
}

func (c *mcpBridgeConn) sendMCPError(id json.RawMessage, code int, message string, data any) error {
	var rawData json.RawMessage

	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}

		rawData = raw
	}

	return c.send(mcpRPCMessage{
		ID: &id,
		Error: &mcpRPCError{
			Code:    code,
			Message: message,
			Data:    rawData,
		},
	})
}

func (c *mcpBridgeConn) send(msg mcpRPCMessage) error {
	msg.JSONRPC = mcpJSONRPCVersion

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	return c.enc.Encode(msg)
}

func (a *Agent) handleMCPMessage(ctx context.Context, params json.RawMessage) (any, *acp.RequestError) {
	var request acp.UnstableMessageMcpRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := request.Validate(); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	conn := a.mcpConnection(request.ConnectionId)
	if conn == nil {
		return nil, acp.NewInvalidParams(map[string]any{"connectionId": request.ConnectionId})
	}

	forwardCtx, stopForward := conn.sessionWorkContext(ctx)
	defer stopForward()

	if isMCPNotificationMethod(request.Method) {
		if err := conn.forwardACPNotification(forwardCtx, request); err != nil {
			return nil, requestError(err)
		}

		return nil, nil
	}

	result, err := conn.forwardACPRequest(forwardCtx, request)
	if err != nil {
		return nil, requestError(err)
	}

	return result, nil
}

func isMCPNotificationMethod(method string) bool {
	return strings.HasPrefix(method, "notifications/") || strings.HasPrefix(method, "$/")
}

func (c *mcpBridgeConn) sessionWorkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.session == nil || c.session.session == "" {
		return ctx, func() {}
	}

	session, err := c.agent.session(c.session.session)
	if err != nil {
		return ctx, func() {}
	}

	return session.sessionWorkContext(ctx)
}

func (c *mcpBridgeConn) forwardACPNotification(ctx context.Context, request acp.UnstableMessageMcpRequest) (err error) {
	start := time.Now()
	result := observer.MCPMessageResult{
		Direction:       "acp_to_proxy",
		Kind:            "notification",
		Method:          request.Method,
		ProtocolVersion: mcpProtocolVersionFromParams(request.Params),
		Transport:       mcpConfigTypeACP,
	}

	ctx, finishSpan := c.agent.observe.StartMCPBridge(ctx, "forward", result)
	defer func() {
		result.Err = err
		finishSpan(err)
		c.agent.observe.RecordMCPBridgeMessage(ctx, start, result)
	}()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	params, err := marshalMCPParams(request.Params)
	if err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return c.send(mcpRPCMessage{
		Method: request.Method,
		Params: params,
	})
}

func (c *mcpBridgeConn) forwardACPRequest(
	ctx context.Context,
	request acp.UnstableMessageMcpRequest,
) (response acp.UnstableMessageMcpResponse, err error) {
	start := time.Now()

	result := observer.MCPMessageResult{
		Direction:       "acp_to_proxy",
		Kind:            jsonFieldRequest,
		Method:          request.Method,
		ProtocolVersion: mcpProtocolVersionFromParams(request.Params),
		Transport:       mcpConfigTypeACP,
	}

	ctx, finishSpan := c.agent.observe.StartMCPBridge(ctx, "forward", result)
	defer func() {
		result.Err = err
		finishSpan(err)
		c.agent.observe.RecordMCPBridgeMessage(ctx, start, result)
	}()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	params, err := marshalMCPParams(request.Params)
	if err != nil {
		return nil, err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()

	id := c.nextID.Add(1)
	rawID := json.RawMessage(strconv.FormatUint(id, 10))
	idKey := mcpIDKey(&rawID)
	ch := make(chan mcpRPCMessage, 1)

	c.mu.Lock()
	if len(c.pending) >= mcpMaxPending {
		c.mu.Unlock()

		return nil, errors.New("too many pending MCP proxy requests")
	}

	c.pending[idKey] = ch
	c.mu.Unlock()

	c.agent.observe.RecordMCPPending(ctx, 1, mcpConfigTypeACP)
	defer c.agent.observe.RecordMCPPending(ctx, -1, mcpConfigTypeACP)

	if err := c.send(mcpRPCMessage{
		ID:     &rawID,
		Method: request.Method,
		Params: params,
	}); err != nil {
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()

		return nil, err
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, errors.New("MCP proxy connection closed")
		}

		if msg.Error != nil {
			return nil, acp.NewInternalError(map[string]any{
				"code":           msg.Error.Code,
				jsonFieldMessage: msg.Error.Message,
			})
		}

		return unmarshalMCPResult(msg.Result)
	case <-requestCtx.Done():
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()

		return nil, requestCtx.Err()
	case <-c.closed:
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()

		return nil, errors.New("MCP proxy connection closed")
	}
}

func mcpMessageKind(msg mcpRPCMessage) string {
	if msg.ID == nil {
		return "notification"
	}

	return jsonFieldRequest
}

func mcpIDKey(id *json.RawMessage) string {
	if id == nil {
		return ""
	}

	return string(bytes.TrimSpace(*id))
}

func marshalMCPParams(params map[string]any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("null"), nil
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	return raw, nil
}

func mcpParamsMap(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}

	var params map[string]any
	if err := json.Unmarshal(trimmed, &params); err != nil {
		return nil, fmt.Errorf("decode MCP params: %w", err)
	}

	return params, nil
}

func mcpProtocolVersionFromRawParams(raw json.RawMessage) string {
	params, err := mcpParamsMap(raw)
	if err != nil {
		return ""
	}

	return mcpProtocolVersionFromParams(params)
}

func mcpProtocolVersionFromParams(params map[string]any) string {
	value, _ := params[mcpFieldProtocolVersion].(string)

	return value
}

func unmarshalMCPResult(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("null"), nil
	}

	var result any
	if err := json.Unmarshal(trimmed, &result); err != nil {
		return nil, fmt.Errorf("decode MCP result: %w", err)
	}

	return result, nil
}
