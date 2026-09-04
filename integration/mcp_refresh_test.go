//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

// claudeMCPRefreshMarker is the sentinel the live turns must echo. It stays
// neutral on purpose: a sentinel that reads like a machine-checkable
// authorization claim makes the model refuse the turn instead of calling the
// tool, and its refusal quotes the sentinel back.
const claudeMCPRefreshMarker = "ACP_MCP_REFRESH_OK"

// TestClaudeCLIAuthorizedMCPRefresh reproduces a session-bound worker proxy:
// session establishment sees only runtime_ready, then the first user turn arms
// execute/search. Claude fixes its MCP registry at process startup, so this
// proves the adapter's one-time pre-query relaunch discovers the authorized
// tools, keeps an unrelated external MCP server registered, and repeats the
// same transition safely after close/load before native continuation.
func TestClaudeCLIAuthorizedMCPRefresh(t *testing.T) {
	requireLiveTokens(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runtime := newClaudeMCPProbe(t, true)
	external := newClaudeMCPProbe(t, false)
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})
	cwd := t.TempDir()
	request := claudeacp.NewSessionRequest(cwd, claudeacp.WithSessionMCPServers(
		claudeacp.HTTPMCPServer("runtime", runtime.server.URL, nil),
		claudeacp.HTTPMCPServer("playwright", external.server.URL, nil),
	))

	session, err := conn.NewSession(ctx, request)
	require.NoError(t, err)
	requireProbeToolList(t, runtime, []string{"runtime_ready"})
	requireProbeToolList(t, external, []string{"browser_navigate"})

	runtime.setArmed(true)
	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, claudeacp.TextPromptRequest(
			session.SessionId,
			"authorized-mcp-turn-1",
			"Call mcp__runtime__execute exactly once with {\"probe\":\"first\"}. "+
				"After the tool returns, reply with exactly "+claudeMCPRefreshMarker+" and no punctuation.",
		))
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), claudeMCPRefreshMarker)
	require.Equal(t, []string{"execute", "runtime_ready", "search"}, runtime.lastToolList())
	require.GreaterOrEqual(t, runtime.listCount(), 2)
	require.GreaterOrEqual(t, external.listCount(), 2, "external MCP must survive the authorized relaunch")
	require.Equal(t, []map[string]any{{"probe": "first"}}, runtime.executeSnapshot())
	firstMessageID := claudeMessageID(resp.Meta)
	require.NotEmpty(t, firstMessageID)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	runtime.setArmed(false)
	client.resetRecordedOutput()

	_, err = conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: request.McpServers,
	})
	require.NoError(t, err)
	requireProbeToolList(t, runtime, []string{"runtime_ready"})
	require.Equal(t, firstMessageID, lastClaudeMessageID(client.updateSnapshot()))

	runtime.setArmed(true)
	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, claudeacp.TextPromptRequest(
			session.SessionId,
			"authorized-mcp-turn-2",
			"Call mcp__runtime__execute exactly once with {\"probe\":\"second\"}. "+
				"After the tool returns, reply with exactly "+claudeMCPRefreshMarker+" and no punctuation.",
		))
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), claudeMCPRefreshMarker)
	require.NotEmpty(t, claudeMessageID(resp.Meta))
	require.Equal(t, []map[string]any{{"probe": "first"}, {"probe": "second"}}, runtime.executeSnapshot())
	require.GreaterOrEqual(t, external.listCount(), 4, "external MCP must remain registered through load and refresh")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

// requireProbeToolList waits for the probe's most recent tools/list to carry
// exactly the wanted names. Claude answers the control initialize request that
// settles a launch before its MCP handshake completes, so the registry a launch
// discovered is only observable after the fact.
func requireProbeToolList(t *testing.T, probe *claudeMCPProbe, want []string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return slices.Equal(probe.lastToolList(), want)
	}, 30*time.Second, 100*time.Millisecond, "probe tool list never settled on %v", want)
}

type claudeMCPProbe struct {
	server *httptest.Server

	mu      sync.Mutex
	dynamic bool
	armed   bool
	lists   [][]string
	execute []map[string]any
}

func newClaudeMCPProbe(t *testing.T, dynamic bool) *claudeMCPProbe {
	t.Helper()

	probe := &claudeMCPProbe{dynamic: dynamic}
	probe.server = httptest.NewServer(probe)
	t.Cleanup(probe.server.Close)

	return probe
}

func (p *claudeMCPProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	var request struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if request.ID == nil {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claude-mcp-refresh-probe", "version": "1"},
		}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		names := p.toolNames()
		p.mu.Lock()
		p.lists = append(p.lists, slices.Clone(names))
		p.mu.Unlock()

		tools := make([]map[string]any, 0, len(names))
		for _, name := range names {
			tools = append(tools, map[string]any{
				"name":        name,
				"description": "Deterministic Claude MCP refresh probe " + name,
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"probe": map[string]any{"type": "string"},
					},
					"additionalProperties": false,
				},
			})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		name, _ := request.Params["name"].(string)
		arguments, _ := request.Params["arguments"].(map[string]any)
		p.mu.Lock()
		armed := p.armed
		if p.dynamic && armed && name == "execute" {
			p.execute = append(p.execute, cloneProbeArguments(arguments))
		}
		p.mu.Unlock()
		if !p.dynamic || !armed || name != "execute" {
			writeClaudeMCPError(w, request.ID, -32602, "tool not authorized")

			return
		}
		result = map[string]any{"content": []any{map[string]any{
			"type": "text", "text": claudeMCPRefreshMarker,
		}}}
	default:
		writeClaudeMCPError(w, request.ID, -32601, fmt.Sprintf("unsupported method %s", request.Method))

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": request.ID, "result": result,
	})
}

func (p *claudeMCPProbe) toolNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.dynamic {
		return []string{"browser_navigate"}
	}
	if p.armed {
		return []string{"execute", "runtime_ready", "search"}
	}

	return []string{"runtime_ready"}
}

func (p *claudeMCPProbe) setArmed(armed bool) {
	p.mu.Lock()
	p.armed = armed
	p.mu.Unlock()
}

func (p *claudeMCPProbe) lastToolList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.lists) == 0 {
		return nil
	}

	return slices.Clone(p.lists[len(p.lists)-1])
}

func (p *claudeMCPProbe) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.lists)
}

func (p *claudeMCPProbe) executeSnapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]map[string]any, 0, len(p.execute))
	for _, arguments := range p.execute {
		out = append(out, cloneProbeArguments(arguments))
	}

	return out
}

func cloneProbeArguments(arguments map[string]any) map[string]any {
	cloned := make(map[string]any, len(arguments))
	for key, value := range arguments {
		cloned[key] = value
	}

	return cloned
}

func writeClaudeMCPError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}
