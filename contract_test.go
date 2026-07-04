package claudeacp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
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
	}
	options, err := claudeOptionsFromMeta(valid)
	require.NoError(t, err)
	require.Equal(t, "sonnet", options.Model)
	require.Equal(t, map[string]any{"type": "object"}, options.OutputSchema)
	require.True(t, rawMessageConfigFromMeta(valid).Enabled())

	tests := []struct {
		name string
		meta map[string]any
	}{
		{
			name: "deleted goal",
			meta: map[string]any{claudeMetaKey: map[string]any{"goal": map[string]any{}}},
		},
		{
			name: "deleted raw sdk",
			meta: map[string]any{claudeMetaKey: map[string]any{"emitRawSDKMessages": true}},
		},
		{
			name: "legacy package key",
			meta: map[string]any{legacyPackageMetaKey: map[string]any{}},
		},
		{
			name: "unknown option",
			meta: map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := claudeOptionsFromMeta(tc.meta)
			require.Error(t, err)
		})
	}
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

func TestRawEventLimit(t *testing.T) {
	require.True(t, rawEventWithinLimit(map[string]any{"ok": true}))
	require.False(t, rawEventWithinLimit(map[string]any{"data": string(make([]byte, rawEventMaxBytes))}))
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
	}{
		{name: "sse", servers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "events"}}}},
		{name: "acp", servers: []acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "bridge"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateMCPServers(tc.servers)
			require.Error(t, err)
			var reqErr *acp.RequestError
			require.ErrorAs(t, err, &reqErr)
			require.Equal(t, -32602, reqErr.Code)
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
