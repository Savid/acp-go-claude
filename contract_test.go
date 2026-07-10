package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestInitializeContractCapabilities(t *testing.T) {
	agent := NewAgent(WithAgentVersion("test"))

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.AuthMethods)
	require.True(t, resp.AgentCapabilities.LoadSession)
	require.True(t, resp.AgentCapabilities.McpCapabilities.Http)
	require.False(t, resp.AgentCapabilities.McpCapabilities.Acp)
	require.False(t, resp.AgentCapabilities.McpCapabilities.Sse)
	require.Nil(t, resp.AgentCapabilities.SessionCapabilities.Fork)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Delete)
	require.Nil(t, resp.AgentCapabilities.Nes)

	claudeMeta := requireAnyMap(t, resp.AgentCapabilities.Meta[claudeMetaKey])
	require.Equal(t, ForkSessionMethod, requireAnyMap(t, claudeMeta["fork"])["method"])
	require.Equal(t, RawEventMethod, requireAnyMap(t, claudeMeta["rawEvent"])["method"])
	require.Equal(t, SessionStoreFormat, requireAnyMap(t, claudeMeta["sessionStore"])["format"])
	require.Equal(t, "_meta.claude.options.outputSchema", requireAnyMap(t, claudeMeta["structuredOutput"])["config"])
}

func TestLifecycleMetaStrictAllowlist(t *testing.T) {
	t.Parallel()

	valid := map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{
				metaModelKey:        "sonnet",
				metaOutputSchemaKey: map[string]any{"type": "object"},
			},
			metaRawEventKey: map[string]any{metaRawEventEnabledKey: true},
		},
		"other": map[string]any{"ignored": true},
		// The full module path is foreign to the owned "claude" namespace and
		// MUST be ignored, never rejected.
		"github.com/savid/acp-go-claude": map[string]any{"ignored": true},
	}
	options, err := claudeOptionsFromMeta(valid)
	require.NoError(t, err)
	require.Equal(t, "sonnet", options.Model)
	require.Equal(t, map[string]any{"type": "object"}, options.OutputSchema)
	require.True(t, rawMessageConfigFromMeta(valid).Enabled())

	tests := []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{
			name:  "deleted goal",
			meta:  map[string]any{claudeMetaKey: map[string]any{"goal": map[string]any{}}},
			field: "_meta.claude.goal",
		},
		{
			name:  "deleted raw sdk",
			meta:  map[string]any{claudeMetaKey: map[string]any{"emitRawSDKMessages": true}},
			field: "_meta.claude.emitRawSDKMessages",
		},
		{
			name:  "unknown option",
			meta:  map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}}},
			field: "_meta.claude.options.extra",
		},
		{
			name:  "unknown raw event key",
			meta:  map[string]any{claudeMetaKey: map[string]any{metaRawEventKey: map[string]any{"extra": true}}},
			field: "_meta.claude.rawEvent.extra",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := claudeOptionsFromMeta(tc.meta)
			requireExactUnsupportedField(t, err, tc.field)
		})
	}
}

func TestLifecycleMetaUnsupportedErrorsPreserveRequestErrorShape(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "new session",
			call: func() error {
				_, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project", WithSessionMeta(map[string]any{
					claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}},
				})))

				return err
			},
		},
		{
			name: "fork extension",
			call: func() error {
				raw, err := json.Marshal(ForkSessionRequest("parent", "/tmp/project", WithSessionMeta(map[string]any{
					claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}},
				})))
				require.NoError(t, err)
				_, err = agent.HandleExtensionMethod(context.Background(), ForkSessionMethod, raw)

				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requireExactUnsupportedField(t, tc.call(), "_meta.claude.options.extra")
		})
	}
}

func requireExactUnsupportedField(t *testing.T, err error, field string) {
	t.Helper()

	require.Error(t, err)
	var reqErr *acp.RequestError
	require.True(t, errors.As(err, &reqErr), "error = %T %[1]v", err)
	require.Equal(t, -32602, reqErr.Code)
	require.Equal(t, "Invalid params", reqErr.Message)
	require.Equal(t, map[string]any{
		jsonFieldError: validationUnsupported,
		jsonFieldField: field,
	}, reqErr.Data)
}

func requireUnknownSession(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	var reqErr *acp.RequestError
	require.True(t, errors.As(err, &reqErr), "error = %T %[1]v", err)
	require.Equal(t, -32602, reqErr.Code)
	require.Equal(t, "Invalid params", reqErr.Message)
	require.Equal(t, map[string]any{
		jsonFieldError: "unknown session",
		jsonFieldField: acpFieldSessionID,
	}, reqErr.Data)
}

func TestInMemorySessionStoreContract(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	main := SessionKey{SessionID: "session-1"}
	sub := SessionKey{SessionID: "session-1", Subpath: "subagents/a"}

	require.NoError(t, store.Append(ctx, main, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	require.NoError(t, store.Append(ctx, sub, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))

	entries, err := store.Load(ctx, main)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	subkeys, err := store.ListSubkeys(ctx, main)
	require.NoError(t, err)
	require.Equal(t, []string{"subagents/a"}, subkeys)

	replacement := []SessionStoreReplacement{{
		Key:     main,
		Entries: []SessionStoreEntry{json.RawMessage(`{"type":"system"}`)},
	}}
	require.NoError(t, store.Replace(ctx, main, replacement))

	subkeys, err = store.ListSubkeys(ctx, main)
	require.NoError(t, err)
	require.Empty(t, subkeys)

	summaries, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, "session-1", summaries[0].SessionID)
	require.Positive(t, summaries[0].UpdatedAtUnixMilli)

	require.NoError(t, store.Delete(ctx, main))
	entries, err = store.Load(ctx, main)
	require.NoError(t, err)
	require.Empty(t, entries)

	summaries, err = store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, summaries)

	require.NoError(t, store.Append(ctx, main, []SessionStoreEntry{json.RawMessage(`{"type":"late"}`)}))
	entries, err = store.Load(ctx, main)
	require.NoError(t, err)
	require.Empty(t, entries)

	require.NoError(t, store.Append(ctx, sub, []SessionStoreEntry{json.RawMessage(`{"type":"late-sub"}`)}))
	entries, err = store.Load(ctx, sub)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDeleteSessionTombstoneHidesNativeTranscriptAndSurfacesCleanupError(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cwd := t.TempDir()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	nativePath := writeNativeTranscript(t, home, cwd, sessionID)

	originalDeleteNativeTranscript := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = originalDeleteNativeTranscript })

	cleanupErr := errors.New("cleanup failed")
	deleteCalls := 0
	deleteNativeTranscript = func(context.Context, string, string) error {
		deleteCalls++

		return cleanupErr
	}

	agent := NewAgent(WithHome(home))
	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.ErrorIs(t, err, cleanupErr)
	require.FileExists(t, nativePath)

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Empty(t, listResp.Sessions)
	require.GreaterOrEqual(t, deleteCalls, 2)
}

func TestDeleteSessionTombstoneSurvivesRestartAndRetriesNativeCleanup(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cwd := t.TempDir()
	sessionID := acp.SessionId("22222222-2222-4222-8222-222222222222")
	nativePath := writeNativeTranscript(t, home, cwd, sessionID)
	store := NewInMemorySessionStore()

	originalDeleteNativeTranscript := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = originalDeleteNativeTranscript })

	cleanupErr := errors.New("cleanup failed")
	deleteNativeTranscript = func(context.Context, string, string) error { return cleanupErr }
	agent := NewAgent(WithHome(home), WithSessionStore(store))
	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.ErrorIs(t, err, cleanupErr)
	require.FileExists(t, nativePath)

	deleteNativeTranscript = originalDeleteNativeTranscript
	restarted := NewAgent(WithHome(home), WithSessionStore(store))

	listResp, err := restarted.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Empty(t, listResp.Sessions)
	require.FileExists(t, nativePath)

	_, err = restarted.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)
	require.NoFileExists(t, nativePath)

	_, err = restarted.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)
}

func writeNativeTranscript(t *testing.T, home string, cwd string, sessionID acp.SessionId) string {
	t.Helper()

	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	path := filepath.Join(home, "projects", projectKey, string(sessionID)+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	entry := map[string]any{
		jsonFieldType: "user",
		jsonFieldCwd:  cwd,
		"message": map[string]any{
			"content": "hello",
		},
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))

	return path
}

func TestRequestBuildersContract(t *testing.T) {
	t.Parallel()

	schema := map[string]any{"type": "object"}
	req := NewSessionRequest(
		"/repo",
		WithSessionOutputSchema(schema),
		WithSessionRawEvents(true),
		WithSessionMCPServers(StdioMCPServer("fs", "server", []string{"--root", "/repo"}, map[string]string{"A": "B"})),
	)
	require.Equal(t, "/repo", req.Cwd)
	require.Len(t, req.McpServers, 1)

	claudeMeta := requireAnyMap(t, req.Meta[claudeMetaKey])
	options := requireAnyMap(t, claudeMeta[metaOptionsKey])
	require.Equal(t, schema, options[metaOutputSchemaKey])
	require.Equal(t, map[string]any{metaRawEventEnabledKey: true}, claudeMeta[metaRawEventKey])

	setModel := SetModelRequest("session-1", "sonnet")
	require.Equal(t, configModel, setModel.ValueId.ConfigId)
	require.Equal(t, acp.SessionConfigValueId("sonnet"), setModel.ValueId.Value)

	deleteReq := DeleteSessionRequest("session-1")
	require.Equal(t, acp.SessionId("session-1"), deleteReq.SessionId)
}

func TestConcurrencyLimitValidation(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: -1}))
	_, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	require.Error(t, err)
}

func TestClientCallConcurrencyLimit(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	release, err := agent.acquireClientCall(context.Background())
	require.NoError(t, err)

	_, err = agent.acquireClientCall(context.Background())
	require.Error(t, err)

	release()
	release, err = agent.acquireClientCall(context.Background())
	require.NoError(t, err)
	release()
}

func TestCancelDuringPromptBypassesClientCallLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	transport := newAutoControlTransport()
	client := claude.NewClient(nil, claude.Options{InitializeTimeout: time.Second}, transport)
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	agent.mu.Lock()
	agent.sessions["session-1"] = &agentSession{
		agent:  agent,
		id:     "session-1",
		client: client,
	}
	agent.mu.Unlock()

	release, err := agent.acquireClientCall(ctx)
	require.NoError(t, err)
	defer release()

	raw, err := json.Marshal(acp.CancelNotification{SessionId: "session-1"})
	require.NoError(t, err)

	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	_, reqErr := conn.handle(ctx, acp.AgentMethodSessionCancel, raw)
	require.Nil(t, reqErr)
}

type autoControlTransport struct {
	incoming chan map[string]any
	errs     chan error

	mu     sync.Mutex
	closed bool
}

func newAutoControlTransport() *autoControlTransport {
	return &autoControlTransport{
		incoming: make(chan map[string]any, 16),
		errs:     make(chan error, 1),
	}
}

func (t *autoControlTransport) Start(context.Context) error { return nil }

func (t *autoControlTransport) Send(_ context.Context, payload any) error {
	if req, ok := payload.(claude.ControlRequest); ok {
		t.incoming <- map[string]any{
			jsonFieldType: "control_response",
			"response": map[string]any{
				"request_id":     req.RequestID,
				jsonFieldSubtype: "success",
			},
		}
	}

	return nil
}

func (t *autoControlTransport) Messages(context.Context) (<-chan map[string]any, <-chan error) {
	return t.incoming, t.errs
}

func (t *autoControlTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.closed {
		close(t.incoming)
		close(t.errs)
		t.closed = true
	}

	return nil
}

func TestRawEventLimit(t *testing.T) {
	_, replaced := rawEventMarker(map[string]any{"ok": true})
	require.False(t, replaced)

	marker, replaced := rawEventMarker(map[string]any{"data": string(make([]byte, rawEventMaxBytes))})
	require.True(t, replaced)
	require.Equal(t, rawEventReasonOversize, marker[rawEventFieldReason])
	require.Equal(t, rawEventMaxBytes, marker[rawEventFieldMaxBytes])
}

func TestSetSessionModeUnsupported(t *testing.T) {
	agent := NewAgent()
	_, err := agent.SetSessionMode(context.Background(), acp.SetSessionModeRequest{})
	require.Error(t, err)
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32601, reqErr.Code)
}

func TestValidateMCPServersRejectsSSEAndACP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers []acp.McpServer
		data    map[string]any
	}{
		{
			name:    "sse",
			servers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "events"}}},
			data: map[string]any{
				jsonFieldError:  validationUnsupported,
				jsonFieldField:  "mcpServers[0]",
				jsonFieldServer: "events",
			},
		},
		{
			name:    "acp",
			servers: []acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "bridge"}}},
			data: map[string]any{
				jsonFieldError:  validationUnsupported,
				jsonFieldField:  "mcpServers[0]",
				jsonFieldServer: "bridge",
			},
		},
		{
			name:    "no transport",
			servers: []acp.McpServer{{}},
			data: map[string]any{
				jsonFieldError: "no_transport",
				jsonFieldField: "mcpServers[0]",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateMCPServers(tc.servers)
			require.Error(t, err)
			var reqErr *acp.RequestError
			require.ErrorAs(t, err, &reqErr)
			require.Equal(t, -32602, reqErr.Code)
			require.Equal(t, tc.data, reqErr.Data)
		})
	}
}

func TestValidateMCPServersNameRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		servers []acp.McpServer
		data    map[string]any
	}{
		{
			name:    "empty name",
			servers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Command: "mcp"}}},
			data:    map[string]any{"mcpServers[0].name": validationRequired},
		},
		{
			name: "whitespace-only name",
			servers: []acp.McpServer{
				{Http: &acp.McpServerHttpInline{Name: "   ", Url: "https://example.com/mcp"}},
			},
			data: map[string]any{"mcpServers[0].name": validationRequired},
		},
		{
			name: "empty name at later index",
			servers: []acp.McpServer{
				{Stdio: &acp.McpServerStdio{Name: "fs", Command: "mcp"}},
				{Http: &acp.McpServerHttpInline{Name: "", Url: "https://example.com/mcp"}},
			},
			data: map[string]any{"mcpServers[1].name": validationRequired},
		},
		{
			name: "duplicate name reports later entry",
			servers: []acp.McpServer{
				{Stdio: &acp.McpServerStdio{Name: "dup", Command: "one"}},
				{Http: &acp.McpServerHttpInline{Name: "dup", Url: "https://example.com/mcp"}},
			},
			data: map[string]any{"mcpServers[1].name": validationDuplicate},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateMCPServers(tc.servers)
			require.Error(t, err)
			var reqErr *acp.RequestError
			require.True(t, errors.As(err, &reqErr), "error = %T %[1]v", err)
			require.Equal(t, -32602, reqErr.Code)
			require.Equal(t, "Invalid params", reqErr.Message)
			require.Equal(t, tc.data, reqErr.Data)
		})
	}
}

func requireAnyMap(t *testing.T, value any) map[string]any {
	t.Helper()

	result, ok := value.(map[string]any)
	require.Truef(t, ok, "expected map[string]any, got %T", value)

	return result
}

func TestStoreOrderingUsesUnixMillis(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "b"}, []SessionStoreEntry{json.RawMessage(`{}`)}))
	time.Sleep(time.Millisecond)
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "a"}, []SessionStoreEntry{json.RawMessage(`{}`)}))

	summaries, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	require.Equal(t, "a", summaries[0].SessionID)
	require.Less(t, summaries[0].UpdatedAtUnixMilli, time.Now().Add(time.Second).UnixMilli())
}
