package claudeacp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestRequestBuilderClones(t *testing.T) {
	t.Parallel()

	meta := map[string]any{"x": []any{map[string]any{"y": "z"}}}
	env := map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"}
	schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}

	req := NewSessionRequest("/repo",
		WithSessionAdditionalDirectories("/extra"),
		WithSessionRawEvents(true),
		WithSessionMeta(meta),
		WithSessionClaudeOptions(NewClaudeOptions(
			WithClaudeModel("sonnet"),
			WithClaudePermissionMode(permissionModeAcceptEdits),
			WithClaudeBare(true),
			WithClaudeEnv(env),
			WithClaudeOutputSchema(schema),
			WithClaudeSystemPrompt("system"),
		)),
	)

	metaX, ok := meta["x"].([]any)
	require.True(t, ok)
	metaX0, ok := metaX[0].(map[string]any)
	require.True(t, ok)
	metaX0["y"] = "changed"
	env["ANTHROPIC_BASE_URL"] = "changed"
	schema["type"] = "changed"

	reqMetaX, ok := req.Meta["x"].([]any)
	require.True(t, ok)
	reqMetaX0, ok := reqMetaX[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "z", reqMetaX0["y"])
	claudeMeta, ok := req.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	options, ok := claudeMeta[metaOptionsKey].(map[string]any)
	require.True(t, ok)
	optionsEnv, ok := options[settingsFieldEnv].(map[string]string)
	require.True(t, ok)
	require.Equal(t, "https://example.test", optionsEnv["ANTHROPIC_BASE_URL"])
	optionsSchema, ok := options[metaOutputSchemaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", optionsSchema["type"])
	require.Equal(t, true, options[metaBareKey])
	require.Equal(t, "sonnet", options[metaModelKey])
	require.Equal(t, permissionModeAcceptEdits, options[metaPermissionModeKey])
	require.Equal(t, "system", options[metaSystemPromptKey])

	require.Empty(t, LoadSessionRequest("s", "/repo").McpServers)
	require.Equal(t, acp.SessionId("s"), ResumeSessionRequest("s", "/repo").SessionId)
	require.Equal(t, acp.SessionId("s"), ForkSessionRequest("s", "/repo").SessionId)
	require.Equal(t, acp.SessionId("s"), DeleteSessionRequest("s").SessionId)
	require.Equal(t, acp.SessionConfigValueId("high"), SetConfigOptionRequest("s", configEffort, "high").ValueId.Value)
	require.Equal(t, acp.SessionConfigValueId("sonnet"), SetModelRequest("s", "sonnet").ValueId.Value)
	require.Len(t, PromptRequest("s", acp.TextBlock("hello")).Prompt, 1)
	require.Equal(t, "hello", TextPromptRequest("s", "hello").Prompt[0].Text.Text)

	list := ListSessionsRequest(WithListSessionsCwd("/repo"), WithListSessionsCursor("c"), WithListSessionsMeta(map[string]any{"a": "b"}))
	require.Equal(t, "/repo", *list.Cwd)
	require.Equal(t, "c", *list.Cursor)
	require.Equal(t, "b", list.Meta["a"])

	stdio := acp.McpServer{Stdio: &acp.McpServerStdio{
		Name: "stdio", Command: "tool", Args: []string{"a"},
		Env:  []acp.EnvVariable{{Name: "A", Value: "B", Meta: map[string]any{"x": "y"}}},
		Meta: map[string]any{"s": "m"},
	}}
	httpServer := acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http", Url: "https://example.test", Headers: []acp.HttpHeader{{Name: "H", Value: "V", Meta: map[string]any{"h": "m"}}}, Meta: map[string]any{"h": "m"}}}
	sse := acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://example.test/sse", Headers: []acp.HttpHeader{{Name: "S", Value: "V"}}, Meta: map[string]any{"s": "m"}}}
	acpServer := acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "a1", Meta: map[string]any{"a": "m"}}}
	sessionReq := NewSessionRequest("/repo", WithSessionMCPServers(stdio, httpServer, sse, acpServer), WithSessionOutputSchema(map[string]any{"type": "object"}))
	require.Len(t, sessionReq.McpServers, 4)

	stdio.Stdio.Args[0] = "mutated"
	stdio.Stdio.Env[0].Value = "mutated"
	httpServer.Http.Headers[0].Value = "mutated"
	require.Equal(t, "a", sessionReq.McpServers[0].Stdio.Args[0])
	require.Equal(t, "B", sessionReq.McpServers[0].Stdio.Env[0].Value)
	require.Equal(t, "V", sessionReq.McpServers[1].Http.Headers[0].Value)

	unstable := unstableMCPServersFromStable(sessionReq.McpServers)
	require.NotNil(t, unstable[0].Stdio)
	require.NotNil(t, unstable[1].Http)
	require.NotNil(t, unstable[2].Sse)
	require.NotNil(t, unstable[3].Acp)
}

func TestRequestBuilderHelperBranches(t *testing.T) {
	t.Parallel()

	require.Nil(t, cloneAny(nil))
	clonedAny, ok := cloneAny(map[string]any{"nested": map[string]any{"x": "y"}}).(map[string]any)
	require.True(t, ok)
	clonedNested, ok := clonedAny["nested"].(map[string]any)
	require.True(t, ok)
	clonedNested["x"] = "changed"
	reclonedAny, ok := cloneAny(map[string]any{"nested": map[string]any{"x": "y"}}).(map[string]any)
	require.True(t, ok)
	reclonedNested, ok := reclonedAny["nested"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "y", reclonedNested["x"])
	originalStrings := []string{"a"}
	clonedStrings, ok := cloneAny(originalStrings).([]string)
	require.True(t, ok)
	clonedStrings[0] = "b"
	require.Equal(t, []string{"a"}, originalStrings)
	require.Nil(t, cloneMCPServers(nil))
	require.Nil(t, cloneMCPServerStdio(nil))
	require.Nil(t, cloneHTTPHeaders(nil))
	require.Nil(t, cloneEnvVariables(nil))
	require.Nil(t, unstableMCPServersFromStable(nil))

	meta := map[string]any{claudeMetaKey: map[string]any{"x": []any{"a"}}}
	cloned := ensureMetaMap(meta, claudeMetaKey)
	clonedX, ok := cloned["x"].([]any)
	require.True(t, ok)
	clonedX[0] = "b"
	metaClaude, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	metaClaudeX, ok := metaClaude["x"].([]any)
	require.True(t, ok)
	require.Equal(t, "b", metaClaudeX[0])

	require.NotNil(t, cloneMCPServer(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Headers: []acp.HttpHeader{{Name: "H"}}}}).Sse)
	require.NotNil(t, cloneMCPServer(acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http", Headers: []acp.HttpHeader{{Name: "H"}}}}).Http)
	require.NotNil(t, cloneMCPServer(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}).Acp)
	require.NotNil(t, cloneMCPServer(acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}).Stdio)
	require.Equal(t, acp.McpServer{}, cloneMCPServer(acp.McpServer{}))
	require.NotNil(t, unstableMCPServerFromStable(acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}).Sse)
	require.NotNil(t, unstableMCPServerFromStable(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}).Acp)
	require.Equal(t, acp.UnstableMcpServer{}, unstableMCPServerFromStable(acp.McpServer{}))

	stdio := StdioMCPServer("stdio", "cmd", []string{"arg"}, map[string]string{"A": "B"})
	require.Equal(t, "arg", stdio.Stdio.Args[0])
	require.Equal(t, "B", stdio.Stdio.Env[0].Value)
	httpServer := HTTPMCPServer("http", "https://example.test", map[string]string{"H": "V"})
	require.Equal(t, "V", httpServer.Http.Headers[0].Value)
}

func TestCallForkSessionHelperDecodeErrors(t *testing.T) {
	ctx := context.Background()
	successR, successW := io.Pipe()
	successA2CR, successA2CW := io.Pipe()
	t.Cleanup(func() {
		_ = successR.Close()
		_ = successW.Close()
		_ = successA2CR.Close()
		_ = successA2CW.Close()
	})
	successClient := acp.NewClientSideConnection(&recordingClient{}, successW, successA2CR)
	_ = acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
		return acp.UnstableForkSessionResponse{SessionId: "child"}, nil
	}, successA2CW, successR)
	resp, err := CallForkSession(ctx, successClient, ForkSessionRequest("parent", "/tmp/project"))
	require.NoError(t, err)
	require.Equal(t, acp.SessionId("child"), resp.SessionId)

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	_ = acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
		return "not a fork response", nil
	}, a2cW, c2aR)
	_, err = CallForkSession(ctx, clientConn, ForkSessionRequest("parent", "/tmp/project"))
	require.Error(t, err)

	errR, errW := io.Pipe()
	errA2CR, errA2CW := io.Pipe()
	t.Cleanup(func() {
		_ = errR.Close()
		_ = errW.Close()
		_ = errA2CR.Close()
		_ = errA2CW.Close()
	})
	errorClient := acp.NewClientSideConnection(&recordingClient{}, errW, errA2CR)
	_ = acp.NewConnection(func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
		return nil, acp.NewMethodNotFound("nope")
	}, errA2CW, errR)
	_, err = CallForkSession(ctx, errorClient, ForkSessionRequest("parent", "/tmp/project"))
	require.Error(t, err)
}

func TestRawMessageConfigBranches(t *testing.T) {
	t.Parallel()

	config := rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{
		metaRawEventKey: map[string]any{metaRawEventEnabledKey: true},
	}})
	require.True(t, config.Enabled())
	require.True(t, config.ShouldEmit(map[string]any{"type": "event"}))
	require.False(t, rawMessageConfigFromMeta(nil).Enabled())
	require.False(t, rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{metaRawEventKey: map[string]any{metaRawEventEnabledKey: false}}}).Enabled())
	require.False(t, config.ShouldEmit(nil))
	require.Nil(t, rawClaudeMessage(nil))
	msg := &claude.SystemMessage{Raw: map[string]any{"type": "system"}}
	require.Equal(t, map[string]any{"type": "system"}, rawClaudeMessage(msg))
	require.True(t, rawEventWithinLimit(map[string]any{"ok": true}))
	require.False(t, rawEventWithinLimit(map[string]any{"bad": func() {}}))
}
