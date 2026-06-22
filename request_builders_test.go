package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

const schemaTypeString = "string"

func TestSessionLifecycleRequestBuilders(t *testing.T) {
	t.Run("session and load default MCP servers to empty slices", func(t *testing.T) {
		newReq := NewSessionRequest("/repo")
		require.NotNil(t, newReq.McpServers)
		require.Empty(t, newReq.McpServers)
		require.NoError(t, newReq.Validate())

		loadReq := LoadSessionRequest("session-1", "/repo")
		require.NotNil(t, loadReq.McpServers)
		require.Empty(t, loadReq.McpServers)
		require.NoError(t, loadReq.Validate())
	})

	t.Run("options clone inputs and merge Claude metadata", func(t *testing.T) {
		servers := []acp.McpServer{
			{
				Stdio: &acp.McpServerStdio{
					Meta:    map[string]any{"server": "meta"},
					Name:    "fs",
					Command: "mcp-fs",
					Args:    []string{"--root", "/repo"},
					Env: []acp.EnvVariable{{
						Meta:  map[string]any{"env": "meta"},
						Name:  "A",
						Value: "1",
					}},
				},
			},
		}
		dirs := []string{"/repo/shared"}
		meta := map[string]any{
			"host": "wagie",
			claudeMetaKey: map[string]any{
				"existing": true,
				metaOptionsKey: map[string]any{
					metaSystemPromptKey: "keep this",
				},
			},
		}
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ok": map[string]any{"type": "boolean"},
			},
		}

		req := NewSessionRequest(
			"/repo",
			WithSessionMeta(meta),
			WithSessionMCPServers(servers...),
			WithSessionAdditionalDirectories(dirs...),
			WithSessionClaudeOptions(ClaudeOptions{Model: "opus"}),
			WithSessionRawSDKMessages(true),
			WithSessionOutputFormat(JSONSchemaOutputFormat(schema)),
		)

		servers[0].Stdio.Args[0] = "changed"
		servers[0].Stdio.Env[0].Meta["env"] = "changed"
		dirs[0] = "/changed"
		meta["host"] = "changed"
		metaClaude := requireAnyMap(t, meta[claudeMetaKey])
		metaClaude["existing"] = false
		schema["type"] = "changed"
		schemaProperties := requireAnyMap(t, schema["properties"])
		schemaOK := requireAnyMap(t, schemaProperties["ok"])
		schemaOK["type"] = schemaTypeString

		require.Equal(t, "/repo", req.Cwd)
		require.Equal(t, []string{"/repo/shared"}, req.AdditionalDirectories)
		require.Equal(t, "--root", req.McpServers[0].Stdio.Args[0])
		require.Equal(t, "meta", req.McpServers[0].Stdio.Env[0].Meta["env"])
		require.Equal(t, "wagie", req.Meta["host"])

		claudeMeta := requireAnyMap(t, req.Meta[claudeMetaKey])
		require.Equal(t, true, claudeMeta["existing"])
		require.Equal(t, true, claudeMeta[emitRawSDKMessagesKey])

		options := requireAnyMap(t, claudeMeta[metaOptionsKey])
		require.Equal(t, "keep this", options[metaSystemPromptKey])
		require.Equal(t, "opus", options[metaModelKey])

		outputFormat := requireAnyMap(t, options[metaOutputFormatKey])
		require.Equal(t, ClaudeOutputFormatJSONSchema, outputFormat[metaOutputFormatTypeKey])
		outputSchema := requireAnyMap(t, outputFormat[metaOutputFormatSchemaKey])
		require.Equal(t, "object", outputSchema["type"])
		properties := requireAnyMap(t, outputSchema["properties"])
		okProperty := requireAnyMap(t, properties["ok"])
		require.Equal(t, "boolean", okProperty["type"])
		require.NoError(t, req.Validate())
	})

	t.Run("later metadata options win on the same leaf", func(t *testing.T) {
		req := NewSessionRequest(
			"/repo",
			WithSessionClaudeOptions(ClaudeOptions{Model: "first"}),
			WithSessionMeta(map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaModelKey: "second"},
				},
			}),
		)

		claudeMeta := requireAnyMap(t, req.Meta[claudeMetaKey])
		options := requireAnyMap(t, claudeMeta[metaOptionsKey])
		require.Equal(t, "second", options[metaModelKey])
	})

	t.Run("resume and fork share lifecycle options", func(t *testing.T) {
		server := acp.McpServer{
			Http: &acp.McpServerHttpInline{
				Meta: map[string]any{"http": "meta"},
				Headers: []acp.HttpHeader{{
					Meta:  map[string]any{"header": "meta"},
					Name:  "Authorization",
					Value: "Bearer token",
				}},
				Name: "remote",
				Type: "http",
				Url:  "https://example.com/mcp",
			},
		}

		resume := ResumeSessionRequest("session-1", "/repo", WithSessionMCPServers(server))
		require.NotNil(t, resume.McpServers)
		require.Equal(t, "remote", resume.McpServers[0].Http.Name)
		require.NoError(t, resume.Validate())

		fork := ForkSessionRequest("session-1", "/repo", WithSessionMCPServers(server))
		require.NotNil(t, fork.McpServers)
		require.Equal(t, "remote", fork.McpServers[0].Http.Name)
		require.Equal(t, "meta", fork.McpServers[0].Http.Headers[0].Meta["header"])
		require.NoError(t, fork.Validate())
	})

	t.Run("MCP variants are cloned for stable and unstable requests", func(t *testing.T) {
		servers := []acp.McpServer{
			{
				Http: &acp.McpServerHttpInline{
					Meta: map[string]any{"kind": "http"},
					Name: "http",
					Type: "http",
					Url:  "https://example.com/http",
				},
			},
			{
				Sse: &acp.McpServerSseInline{
					Meta: map[string]any{"kind": "sse"},
					Name: "sse",
					Type: "sse",
					Url:  "https://example.com/sse",
				},
			},
			{
				Acp: &acp.McpServerAcpInline{
					Meta: map[string]any{"kind": "acp"},
					Id:   "acp-id",
					Name: "acp",
					Type: "acp",
				},
			},
			{
				Stdio: &acp.McpServerStdio{
					Meta:    map[string]any{"kind": "stdio"},
					Name:    "stdio",
					Command: "stdio",
				},
			},
			{},
		}

		newReq := NewSessionRequest("/repo", WithSessionMCPServers(servers...))
		require.Equal(t, "http", newReq.McpServers[0].Http.Name)
		require.Equal(t, "sse", newReq.McpServers[1].Sse.Name)
		require.Equal(t, acp.McpServerAcpId("acp-id"), newReq.McpServers[2].Acp.Id)
		require.Equal(t, "stdio", newReq.McpServers[3].Stdio.Command)
		require.Equal(t, acp.McpServer{}, newReq.McpServers[4])

		forkReq := ForkSessionRequest("session-1", "/repo", WithSessionMCPServers(servers...))
		require.Equal(t, "http", forkReq.McpServers[0].Http.Name)
		require.Equal(t, "sse", forkReq.McpServers[1].Sse.Name)
		require.Equal(t, acp.UnstableMcpServerAcpId("acp-id"), forkReq.McpServers[2].Acp.Id)
		require.Equal(t, "stdio", forkReq.McpServers[3].Stdio.Command)
		require.Equal(t, acp.UnstableMcpServer{}, forkReq.McpServers[4])

		servers[0].Http.Meta["kind"] = "changed"
		servers[1].Sse.Meta["kind"] = "changed"
		servers[2].Acp.Meta["kind"] = "changed"
		servers[3].Stdio.Meta["kind"] = "changed"

		require.Equal(t, "http", newReq.McpServers[0].Http.Meta["kind"])
		require.Equal(t, "sse", newReq.McpServers[1].Sse.Meta["kind"])
		require.Equal(t, "acp", newReq.McpServers[2].Acp.Meta["kind"])
		require.Equal(t, "stdio", newReq.McpServers[3].Stdio.Meta["kind"])
		require.Equal(t, "http", forkReq.McpServers[0].Http.Meta["kind"])
		require.Equal(t, "sse", forkReq.McpServers[1].Sse.Meta["kind"])
		require.Equal(t, "acp", forkReq.McpServers[2].Acp.Meta["kind"])
		require.Equal(t, "stdio", forkReq.McpServers[3].Stdio.Meta["kind"])
	})

	t.Run("raw message metadata initializes Claude meta", func(t *testing.T) {
		req := NewSessionRequest("/repo", WithSessionRawSDKMessages(false))
		claudeMeta := requireAnyMap(t, req.Meta[claudeMetaKey])
		require.Equal(t, false, claudeMeta[emitRawSDKMessagesKey])
	})
}

func TestPromptRequestBuilders(t *testing.T) {
	empty := PromptRequest("session-1")
	require.Equal(t, acp.SessionId("session-1"), empty.SessionId)
	require.NotNil(t, empty.Prompt)
	require.Empty(t, empty.Prompt)
	require.NoError(t, empty.Validate())

	text := TextPromptRequest("session-1", "hello")
	require.Len(t, text.Prompt, 1)
	require.Equal(t, "hello", text.Prompt[0].Text.Text)
	require.NoError(t, text.Validate())
}

func TestClaudeOptionsBuilderAndOutputFormat(t *testing.T) {
	env := map[string]string{"A": "1"}
	dirs := []string{"/repo/shared"}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": schemaTypeString},
		},
	}

	options := NewClaudeOptions(
		WithClaudeBare(true),
		WithClaudeEnv(env),
		WithClaudeSystemPrompt("system"),
		WithClaudeModel("opus"),
		WithClaudePermissionMode("acceptEdits"),
		WithClaudeAdditionalDirectories(dirs...),
		WithClaudeJSONSchema(schema),
	)

	env["A"] = "changed"
	dirs[0] = "/changed"
	schema["type"] = "changed"
	schemaProperties := requireAnyMap(t, schema["properties"])
	schemaName := requireAnyMap(t, schemaProperties["name"])
	schemaName["type"] = "number"

	require.True(t, options.Bare)
	require.Equal(t, map[string]string{"A": "1"}, options.Env)
	require.Equal(t, "system", options.SystemPrompt)
	require.Equal(t, "opus", options.Model)
	require.Equal(t, "acceptEdits", options.PermissionMode)
	require.Equal(t, []string{"/repo/shared"}, options.AdditionalDirectories)
	require.NotNil(t, options.OutputFormat)
	require.Equal(t, ClaudeOutputFormatJSONSchema, options.OutputFormat.Type)
	require.Equal(t, "object", options.OutputFormat.Schema["type"])

	properties := requireAnyMap(t, options.OutputFormat.Schema["properties"])
	nameProperty := requireAnyMap(t, properties["name"])
	require.Equal(t, schemaTypeString, nameProperty["type"])

	format := JSONSchemaOutputFormat(map[string]any{"type": "object"})
	require.Equal(t, ClaudeOutputFormatJSONSchema, format.Type)
	require.Equal(t, map[string]any{"type": "object"}, format.Schema)
}

func TestListSessionsRequestBuilder(t *testing.T) {
	meta := map[string]any{"host": map[string]any{"name": "wagie"}}

	req := ListSessionsRequest(
		WithListSessionsCwd("/repo"),
		WithListSessionsCursor("cursor-1"),
		WithListSessionsMeta(meta),
	)

	hostMeta := requireAnyMap(t, meta["host"])
	hostMeta["name"] = "changed"

	require.NotNil(t, req.Cwd)
	require.Equal(t, "/repo", *req.Cwd)
	require.NotNil(t, req.Cursor)
	require.Equal(t, "cursor-1", *req.Cursor)
	requestHostMeta := requireAnyMap(t, req.Meta["host"])
	require.Equal(t, "wagie", requestHostMeta["name"])
	require.NoError(t, req.Validate())
}

func TestRequestBuilderNilCloneBranches(t *testing.T) {
	require.Nil(t, cloneMCPServers(nil))
	require.Nil(t, unstableMCPServersFromStable(nil))
	require.Nil(t, cloneMCPServerStdio(nil))
	require.Nil(t, cloneHTTPHeaders(nil))
	require.Nil(t, cloneEnvVariables(nil))
}

func requireAnyMap(t *testing.T, value any) map[string]any {
	t.Helper()

	typed, ok := value.(map[string]any)
	require.True(t, ok)

	return typed
}
