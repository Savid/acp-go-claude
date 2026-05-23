package claudeacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	mcpProxySubcommand      = "mcp-proxy"
	mcpProxyNetwork         = "tcp"
	mcpProxyHost            = "127.0.0.1:0"
	mcpProxyTokenBytes      = 32
	mcpProxyTimeout         = 30 * time.Second
	mcpProxyInitialBuf      = 1024 * 1024
	mcpProxyMaxBuf          = 10 * 1024 * 1024
	mcpJSONRPCVersion       = "2.0"
	mcpProxyVersion         = 1
	mcpMaxForwards          = 64
	mcpMaxPending           = 64
	mcpAcceptBackoffInitial = 10 * time.Millisecond
	mcpAcceptBackoffMax     = 250 * time.Millisecond
	mcpMessageKindHello     = "hello"
	mcpFieldProtocolVersion = "protocolVersion"

	// MCPProxyTokenFileEnv carries the path to a file containing the bridge token.
	// #nosec G101 -- this is the environment variable name, not the token value.
	MCPProxyTokenFileEnv = "ACP_GO_CLAUDE_MCP_PROXY_TOKEN_FILE"
)

var (
	currentExecutable           = os.Executable
	mcpRandReader     io.Reader = rand.Reader
	mcpNetListen                = net.Listen
	mcpDialContext              = (&net.Dialer{}).DialContext
	mcpCreateTemp               = func(dir string, pattern string) (mcpTokenTempFile, error) {
		return os.CreateTemp(dir, pattern)
	}
)

type mcpTokenTempFile interface {
	io.Closer
	Name() string
	Sync() error
	WriteString(string) (int, error)
}

type mcpSessionBridge struct {
	agent     *Agent
	session   acp.SessionId
	token     string
	tokenFile string
	command   string
	args      []string
	allowed   map[string]struct{}
	ln        net.Listener
	cancel    context.CancelFunc
	timeout   time.Duration

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup

	mu    sync.Mutex
	conns map[*mcpBridgeConn]struct{}
}

type mcpBridgeConn struct {
	agent        *Agent
	session      *mcpSessionBridge
	conn         net.Conn
	dec          *json.Decoder
	enc          *json.Encoder
	connectionID acp.UnstableMcpConnectionId
	started      time.Time
	timeout      time.Duration

	nextID  atomic.Uint64
	writeMu sync.Mutex

	mu       sync.Mutex
	pending  map[string]chan mcpRPCMessage
	forwards chan struct{}
	closing  bool
	closed   chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

type mcpProxyHello struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	ACPID   string `json:"acpId"`
}

type mcpRPCMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *mcpRPCError     `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (a *Agent) prepareMCPServers(
	ctx context.Context,
	sessionID acp.SessionId,
	servers []acp.McpServer,
) ([]acp.McpServer, *mcpSessionBridge, error) {
	if !hasACPMCPServer(servers) {
		return servers, nil, nil
	}

	if a.connection() == nil {
		return nil, nil, errors.New("ACP connection is required for ACP-transport MCP servers")
	}

	allowed := make(map[string]struct{})

	for _, server := range servers {
		if server.Acp != nil {
			allowed[string(server.Acp.Id)] = struct{}{}
		}
	}

	bridge, err := a.newMCPSessionBridge(ctx, sessionID, allowed)
	if err != nil {
		return nil, nil, err
	}

	out := make([]acp.McpServer, 0, len(servers))
	for _, server := range servers {
		if server.Acp == nil {
			out = append(out, server)

			continue
		}

		out = append(out, acp.McpServer{
			Stdio: &acp.McpServerStdio{
				Name:    server.Acp.Name,
				Command: bridge.command,
				Args:    bridge.proxyArgs(string(server.Acp.Id)),
				Env: []acp.EnvVariable{
					{Name: MCPProxyTokenFileEnv, Value: bridge.tokenFile},
				},
			},
		})
	}

	return out, bridge, nil
}

func hasACPMCPServer(servers []acp.McpServer) bool {
	for _, server := range servers {
		if server.Acp != nil {
			return true
		}
	}

	return false
}

func (a *Agent) newMCPSessionBridge(
	ctx context.Context,
	sessionID acp.SessionId,
	allowedACPIDs ...map[string]struct{},
) (*mcpSessionBridge, error) {
	command := strings.TrimSpace(a.options.MCPProxyCommand)
	if command == "" {
		executable, err := currentExecutable()
		if err != nil {
			return nil, fmt.Errorf("resolve MCP proxy executable: %w", err)
		}

		command = executable
	}

	token, err := newMCPProxyToken()
	if err != nil {
		return nil, err
	}

	tokenFile, err := writeMCPProxyTokenFile(token)
	if err != nil {
		return nil, err
	}

	ln, err := mcpNetListen(mcpProxyNetwork, mcpProxyHost)
	if err != nil {
		_ = os.Remove(tokenFile)

		return nil, fmt.Errorf("listen for MCP proxy: %w", err)
	}

	bridgeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	bridge := &mcpSessionBridge{
		agent:     a,
		session:   sessionID,
		token:     token,
		tokenFile: tokenFile,
		command:   command,
		args:      append([]string(nil), a.options.MCPProxyArgs...),
		allowed:   cloneMCPAllowedACPIDs(allowedACPIDs...),
		ln:        ln,
		cancel:    cancel,
		timeout:   mcpProxyTimeout,
		done:      make(chan struct{}),
		conns:     make(map[*mcpBridgeConn]struct{}),
	}

	bridge.wg.Add(1)
	go func() {
		defer bridge.recoverAccept(bridgeCtx)
		defer bridge.wg.Done()

		bridge.accept(bridgeCtx)
	}()

	return bridge, nil
}

func (b *mcpSessionBridge) recoverAccept(ctx context.Context) {
	handleAgentGoroutinePanic(ctx, b.agent.log, "MCP bridge accept", func(any) {
		if err := b.Close(); err != nil {
			b.agent.log.DebugContext(ctx, "close MCP bridge after panic failed", slog.String(jsonFieldError, err.Error()))
		}
	}, recover())
}

func (b *mcpSessionBridge) recoverConnection(ctx context.Context) {
	handleAgentGoroutinePanic(ctx, b.agent.log, "MCP bridge connection", func(any) {
		if err := b.Close(); err != nil {
			b.agent.log.DebugContext(ctx, "close MCP bridge after connection panic failed", slog.String(jsonFieldError, err.Error()))
		}
	}, recover())
}

func cloneMCPAllowedACPIDs(sets ...map[string]struct{}) map[string]struct{} {
	allowed := make(map[string]struct{})

	for _, set := range sets {
		for id := range set {
			if id != "" {
				allowed[id] = struct{}{}
			}
		}
	}

	return allowed
}

func newMCPProxyToken() (string, error) {
	buf := make([]byte, mcpProxyTokenBytes)
	if _, err := io.ReadFull(mcpRandReader, buf); err != nil {
		return "", fmt.Errorf("generate MCP proxy token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writeMCPProxyTokenFile(token string) (string, error) {
	file, err := mcpCreateTemp("", "acp-go-claude-mcp-token-*")
	if err != nil {
		return "", fmt.Errorf("create MCP proxy token file: %w", err)
	}

	name := file.Name()
	if _, err := file.WriteString(token); err != nil {
		_ = file.Close()
		_ = os.Remove(name)

		return "", fmt.Errorf("write MCP proxy token file: %w", err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(name)

		return "", fmt.Errorf("sync MCP proxy token file: %w", err)
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(name)

		return "", fmt.Errorf("close MCP proxy token file: %w", err)
	}

	return name, nil
}

func (b *mcpSessionBridge) proxyArgs(acpID string) []string {
	args := append([]string(nil), b.args...)
	args = append(args,
		mcpProxySubcommand,
		"-network", b.ln.Addr().Network(),
		"-address", b.ln.Addr().String(),
		"-acp-id", acpID,
	)

	return args
}

func (b *mcpSessionBridge) accept(ctx context.Context) {
	acceptBackoff := mcpAcceptBackoffInitial

	for {
		conn, err := b.ln.Accept()
		if err != nil {
			select {
			case <-b.done:
				return
			default:
			}

			b.agent.log.DebugContext(ctx, "accept MCP proxy connection failed", slog.String(jsonFieldError, err.Error()))

			if !retryableAcceptError(err) {
				return
			}

			if !waitMCPAcceptRetry(b.done, acceptBackoff) {
				return
			}

			acceptBackoff = nextMCPAcceptBackoff(acceptBackoff)

			continue
		}

		acceptBackoff = mcpAcceptBackoffInitial

		b.wg.Add(1)
		go func() {
			defer b.recoverConnection(ctx)
			defer b.wg.Done()

			b.handleConn(ctx, conn)
		}()
	}
}

type temporaryError interface {
	Temporary() bool
}

// MCPProxyOptions configures RunMCPProxy for the internal mcp-proxy subcommand.
type MCPProxyOptions struct {
	// Network is the bridge listener network, usually "tcp".
	Network string
	// Address is the bridge listener address.
	Address string
	// Token authenticates the stdio shim to the bridge.
	Token string
	// ACPID identifies the ACP MCP server being proxied.
	ACPID string
}
