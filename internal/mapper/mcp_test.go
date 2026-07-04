package mapper

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestMCPServersToClaude(t *testing.T) {
	t.Parallel()

	empty, err := MCPServersToClaude(nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	config, err := MCPServersToClaude([]acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    "fs",
				Command: "mcp-fs",
				Args:    []string{"--root", "/tmp"},
				Env:     []acp.EnvVariable{{Name: "A", Value: "B"}},
			},
		},
		{
			Http: &acp.McpServerHttpInline{
				Name:    "http",
				Url:     "https://example.com/mcp",
				Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer token"}},
			},
		},
		{
			Stdio: &acp.McpServerStdio{
				Name:    "stdio-empty",
				Command: "mcp-empty",
			},
		},
	})

	require.NoError(t, err)

	var decoded map[string]map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(config), &decoded))
	require.Equal(t, "mcp-fs", decoded["mcpServers"]["fs"]["command"])
	require.Equal(t, "http", decoded["mcpServers"]["http"]["type"])
	require.NotContains(t, decoded["mcpServers"]["stdio-empty"], "args")
	require.NotContains(t, decoded["mcpServers"]["stdio-empty"], "env")
}

func TestMCPServersToClaudeUnsupported(t *testing.T) {
	t.Parallel()

	_, err := MCPServersToClaude([]acp.McpServer{{Acp: &acp.McpServerAcpInline{Name: "acp", Id: "id"}}})
	require.Error(t, err)

	_, err = MCPServersToClaude([]acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "events", Url: "https://example.com/sse"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SSE MCP")

	_, err = MCPServersToClaude([]acp.McpServer{{}})
	require.Error(t, err)

	_, err = MCPServersToClaude([]acp.McpServer{{Stdio: &acp.McpServerStdio{Command: "mcp"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty name")

	_, err = MCPServersToClaude([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "dup", Command: "one"}},
		{Http: &acp.McpServerHttpInline{Name: "dup", Url: "https://example.com/mcp"}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestMCPServersToClaudeMarshalError(t *testing.T) {
	marshal := marshalMCPConfig
	marshalMCPConfig = func(any) ([]byte, error) {
		return nil, errors.New("marshal failed")
	}
	t.Cleanup(func() {
		marshalMCPConfig = marshal
	})

	_, err := MCPServersToClaude([]acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    "fs",
				Command: "mcp-fs",
			},
		},
	})
	require.Error(t, err)
}

func TestStableMCPServers(t *testing.T) {
	t.Parallel()

	empty, err := StableMCPServers(nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	stable, err := StableMCPServers([]acp.UnstableMcpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    "fs",
				Command: "mcp-fs",
				Args:    []string{"--root", "/tmp"},
			},
		},
		{
			Http: &acp.UnstableMcpServerHttp{
				Name: "http",
				Url:  "https://example.com/mcp",
			},
		},
		{
			Sse: &acp.UnstableMcpServerSse{
				Name:    "events",
				Url:     "https://example.com/sse",
				Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer token"}},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, stable, 3)

	_, err = MCPServersToClaude(stable)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SSE MCP")

	config, err := MCPServersToClaude(stable[:2])
	require.NoError(t, err)

	var decoded map[string]map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(config), &decoded))
	require.Equal(t, "mcp-fs", decoded["mcpServers"]["fs"]["command"])
	require.Equal(t, "http", decoded["mcpServers"]["http"]["type"])

	_, err = StableMCPServers([]acp.UnstableMcpServer{{}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported unstable MCP server")

	stable, err = StableMCPServers([]acp.UnstableMcpServer{
		{Acp: &acp.UnstableMcpServerAcpInline{Name: "acp", Id: "id"}},
	})
	require.NoError(t, err)
	require.Equal(t, acp.McpServerAcpId("id"), stable[0].Acp.Id)
}
