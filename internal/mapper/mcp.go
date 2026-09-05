package mapper

import (
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

var marshalMCPConfig = json.Marshal

// MCPServersToClaude converts ACP MCP server declarations to Claude --mcp-config JSON.
func MCPServersToClaude(servers []acp.McpServer) (string, error) {
	if len(servers) == 0 {
		return "", nil
	}

	out := make(map[string]any, len(servers))
	for _, server := range servers {
		name, config, err := mcpServerToClaude(server)
		if err != nil {
			return "", err
		}

		out[name] = config
	}

	data, err := marshalMCPConfig(map[string]any{keyMCPServers: out})
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}

	return string(data), nil
}

// StableMCPServers converts unstable MCP declarations to the stable shape.
// UnsupportedMCPServerError names the request index of an unstable MCP server
// entry that carries no transport this mapper can render in the stable form. The
// index is what a caller needs to name the offending request member back.
type UnsupportedMCPServerError struct{ Index int }

// Error implements error.
func (e *UnsupportedMCPServerError) Error() string {
	return fmt.Sprintf("unsupported unstable MCP server at index %d", e.Index)
}

func StableMCPServers(servers []acp.UnstableMcpServer) ([]acp.McpServer, error) {
	out := make([]acp.McpServer, 0, len(servers))
	for i, server := range servers {
		switch {
		case server.Stdio != nil:
			out = append(out, acp.McpServer{Stdio: server.Stdio})
		case server.Http != nil:
			out = append(out, acp.McpServer{
				Http: &acp.McpServerHttpInline{
					Meta:    server.Http.Meta,
					Headers: server.Http.Headers,
					Name:    server.Http.Name,
					Type:    server.Http.Type,
					Url:     server.Http.Url,
				},
			})
		case server.Sse != nil:
			out = append(out, acp.McpServer{
				Sse: &acp.McpServerSseInline{
					Meta:    server.Sse.Meta,
					Headers: server.Sse.Headers,
					Name:    server.Sse.Name,
					Type:    server.Sse.Type,
					Url:     server.Sse.Url,
				},
			})
		case server.Acp != nil:
			out = append(out, acp.McpServer{
				Acp: &acp.McpServerAcpInline{
					Meta: server.Acp.Meta,
					Id:   acp.McpServerAcpId(server.Acp.Id),
					Name: server.Acp.Name,
					Type: server.Acp.Type,
				},
			})
		default:
			return nil, &UnsupportedMCPServerError{Index: i}
		}
	}

	return out, nil
}

func mcpServerToClaude(server acp.McpServer) (string, map[string]any, error) {
	switch {
	case server.Stdio != nil:
		env := make(map[string]string, len(server.Stdio.Env))
		for _, variable := range server.Stdio.Env {
			env[variable.Name] = variable.Value
		}

		config := map[string]any{
			keyType:    typeStdio,
			keyCommand: server.Stdio.Command,
		}
		if len(server.Stdio.Args) > 0 {
			config[keyArgs] = server.Stdio.Args
		}

		if len(env) > 0 {
			config[keyEnv] = env
		}

		return server.Stdio.Name, config, nil
	case server.Http != nil:
		config := map[string]any{
			keyType: typeHTTP,
			keyURL:  server.Http.Url,
		}
		if headers := headersToMap(server.Http.Headers); len(headers) > 0 {
			config[keyHeaders] = headers
		}

		return server.Http.Name, config, nil
	case server.Sse != nil:
		return "", nil, fmt.Errorf("SSE MCP servers are not supported")
	case server.Acp != nil:
		return "", nil, fmt.Errorf("ACP-transport MCP servers are not supported by Claude Code")
	default:
		return "", nil, fmt.Errorf("empty MCP server")
	}
}

func headersToMap(headers []acp.HttpHeader) map[string]string {
	out := make(map[string]string, len(headers))
	for _, header := range headers {
		out[header.Name] = header.Value
	}

	return out
}
