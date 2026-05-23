package claudeacp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/observer"
)

var (
	errMCPProxyHelloRejected     = errors.New("MCP proxy hello rejected")
	errMCPProxyUnsupportedHello  = errors.New("MCP proxy hello version unsupported")
	errMCPProxyUnauthorizedACPID = errors.New("MCP proxy ACP ID unauthorized")
)

func retryableAcceptError(err error) bool {
	var temporary temporaryError

	return errors.As(err, &temporary) && temporary.Temporary()
}

func nextMCPAcceptBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return mcpAcceptBackoffInitial
	}

	if current >= mcpAcceptBackoffMax/2 {
		return mcpAcceptBackoffMax
	}

	return current * 2
}

func waitMCPAcceptRetry(done <-chan struct{}, delay time.Duration) bool {
	if delay <= 0 {
		delay = mcpAcceptBackoffInitial
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-done:
		return false
	case <-timer.C:
		return true
	}
}

func (b *mcpSessionBridge) handleConn(ctx context.Context, conn net.Conn) {
	ctx, finishConnect := b.agent.observe.StartMCPBridge(ctx, "connect", observer.MCPMessageResult{
		Direction: "proxy_to_acp",
		Kind:      mcpMessageKindHello,
		Transport: mcpConfigTypeACP,
	})

	var connectErr error
	defer func() { finishConnect(connectErr) }()

	_ = conn.SetReadDeadline(time.Now().Add(mcpProxyTimeout))
	dec := newMCPBridgeDecoder(conn)

	var hello mcpProxyHello
	if err := dec.Decode(&hello); err != nil {
		connectErr = err
		b.agent.log.DebugContext(ctx, "decode MCP proxy hello failed", slog.String(jsonFieldError, err.Error()))

		_ = conn.Close()

		return
	}

	_ = conn.SetReadDeadline(time.Time{})

	tokenMatch := constantTimeStringEqual(hello.Token, b.token)
	if !tokenMatch || hello.ACPID == "" {
		connectErr = errMCPProxyHelloRejected

		b.agent.log.DebugContext(ctx, "reject MCP proxy hello", slog.Bool("token_match", tokenMatch))

		_ = conn.Close()

		return
	}

	if hello.Version != mcpProxyVersion {
		connectErr = errMCPProxyUnsupportedHello

		b.agent.log.WarnContext(
			ctx,
			"reject MCP proxy hello with unsupported version",
			slog.Int("version", hello.Version),
			slog.Int("supported_version", mcpProxyVersion),
		)

		_ = conn.Close()

		return
	}

	if !b.allowsACPID(hello.ACPID) {
		connectErr = errMCPProxyUnauthorizedACPID

		b.agent.log.DebugContext(ctx, "reject MCP proxy hello with unauthorized ACP ID", slog.String("acp_id", hello.ACPID))

		_ = conn.Close()

		return
	}

	// The token authenticates the local proxy connection at handshake. Later MCP
	// messages rely on this private loopback connection staying bound to that
	// authenticated proxy process.

	acpConn := b.agent.connection()
	if acpConn == nil {
		connectErr = errACPConnectionNotAttached
		_ = conn.Close()

		return
	}

	connectCtx, cancel := context.WithTimeout(ctx, mcpProxyTimeout)
	defer cancel()

	resp, err := acpConn.UnstableConnectMcp(connectCtx, acp.UnstableConnectMcpRequest{
		AcpId: acp.UnstableMcpServerAcpId(hello.ACPID),
	})
	if err != nil {
		connectErr = err
		b.agent.log.WarnContext(ctx, "connect ACP MCP failed", slog.String(jsonFieldError, err.Error()))

		_ = conn.Close()

		return
	}

	proxy := &mcpBridgeConn{
		agent:        b.agent,
		session:      b,
		conn:         conn,
		dec:          dec,
		enc:          json.NewEncoder(conn),
		connectionID: resp.ConnectionId,
		started:      time.Now(),
		timeout:      b.timeout,
		pending:      make(map[string]chan mcpRPCMessage),
		forwards:     make(chan struct{}, mcpMaxForwards),
		closed:       make(chan struct{}),
	}

	if !b.addConn(proxy) {
		connectErr = errors.New("MCP bridge is closed")

		proxy.close(ctx)

		return
	}

	b.agent.registerMCPConnection(proxy)

	connCtx, cancel := proxy.connectionContext(ctx)
	defer cancel()

	proxy.run(connCtx)
}

func (c *mcpBridgeConn) connectionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	connCtx, cancel := context.WithCancel(ctx)

	c.wg.Add(1)
	go func() {
		defer c.recoverForward(ctx, "MCP connection context watcher")
		defer c.wg.Done()

		select {
		case <-c.closed:
			cancel()
		case <-connCtx.Done():
		}
	}()

	return connCtx, cancel
}

func (b *mcpSessionBridge) allowsACPID(id string) bool {
	_, ok := b.allowed[id]

	return ok
}

func newMCPBridgeDecoder(reader io.Reader) *json.Decoder {
	// The stdlib decoder has no depth limit knob; this caps total bridge input.
	// Schema/depth tolerance stays with the MCP/JSON-RPC handlers.
	return json.NewDecoder(io.LimitReader(reader, mcpProxyMaxBuf))
}

func (b *mcpSessionBridge) addConn(conn *mcpBridgeConn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.done:
		return false
	default:
	}

	b.conns[conn] = struct{}{}

	return true
}

func (b *mcpSessionBridge) removeConn(conn *mcpBridgeConn) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.conns, conn)
}

func (b *mcpSessionBridge) Close() error {
	var closeErr error

	b.closeOnce.Do(func() {
		close(b.done)

		if b.cancel != nil {
			b.cancel()
		}

		if b.ln != nil {
			closeErr = errors.Join(closeErr, b.ln.Close())
		}

		b.mu.Lock()

		conns := make([]*mcpBridgeConn, 0, len(b.conns))
		for conn := range b.conns {
			conns = append(conns, conn)
		}
		b.mu.Unlock()

		for _, conn := range conns {
			conn.Close()
		}

		b.wg.Wait()

		if b.tokenFile != "" {
			if err := os.Remove(b.tokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				closeErr = errors.Join(closeErr, err)
			}
		}
	})

	return closeErr
}

func (a *Agent) registerMCPConnection(conn *mcpBridgeConn) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.mcpConnections[conn.connectionID] = conn
}

func (a *Agent) unregisterMCPConnection(conn *mcpBridgeConn) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.mcpConnections[conn.connectionID] == conn {
		delete(a.mcpConnections, conn.connectionID)
	}
}

func constantTimeStringEqual(left string, right string) bool {
	if len(left) != len(right) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (a *Agent) mcpConnection(connectionID acp.UnstableMcpConnectionId) *mcpBridgeConn {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.mcpConnections[connectionID]
}

func (c *mcpBridgeConn) run(ctx context.Context) {
	defer c.close(ctx)
	defer c.recoverProxyReader(ctx)

	for {
		var msg mcpRPCMessage
		if err := c.dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				c.agent.log.DebugContext(ctx, "read MCP proxy message failed", slog.String(jsonFieldError, err.Error()))
			}

			return
		}

		switch {
		case msg.ID != nil && msg.Method == "":
			c.handleProxyResponse(msg)
		case msg.Method != "":
			c.forwardProxyMessageAsync(ctx, msg)
		default:
			c.agent.log.DebugContext(ctx, "ignore invalid MCP proxy message")
		}
	}
}

func (c *mcpBridgeConn) recoverProxyReader(ctx context.Context) {
	handleAgentGoroutinePanic(ctx, c.agent.log, "MCP proxy reader", func(any) {
		c.Close()
	}, recover())
}

func (c *mcpBridgeConn) Close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()

		close(c.closed)
		_ = c.conn.Close()

		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()

		c.wg.Wait()
	})
}

func (c *mcpBridgeConn) close(ctx context.Context) {
	c.Close()
	c.session.removeConn(c)
	c.agent.unregisterMCPConnection(c)
	c.agent.observe.RecordMCPSession(ctx, c.started, observer.MCPMessageResult{Transport: mcpConfigTypeACP})

	acpConn := c.agent.connection()
	if acpConn == nil {
		return
	}

	disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpProxyTimeout)
	defer cancel()

	if _, err := acpConn.UnstableDisconnectMcp(disconnectCtx, acp.UnstableDisconnectMcpRequest{
		ConnectionId: c.connectionID,
	}); err != nil {
		c.agent.log.DebugContext(ctx, "disconnect ACP MCP failed", slog.String(jsonFieldError, err.Error()))
	}
}
