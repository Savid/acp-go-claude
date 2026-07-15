package claudeacp

import (
	"fmt"
	"os"
	"path/filepath"
)

var (
	mcpMkdirTemp = os.MkdirTemp
	mcpWriteFile = os.WriteFile
	mcpRemoveAll = os.RemoveAll
)

func writeSessionMCPConfig(scratchDir, config string) (path, dir string, err error) {
	if config == "" {
		return "", "", nil
	}

	parent, err := ensureScratchParent(scratchDir)
	if err != nil {
		return "", "", err
	}

	dir, err = mcpMkdirTemp(parent, "acp-go-claude-mcp-*")
	if err != nil {
		return "", "", fmt.Errorf("create Claude MCP config dir: %w", err)
	}

	path = filepath.Join(dir, "mcp.json")
	if err := mcpWriteFile(path, []byte(config), 0o600); err != nil {
		_ = mcpRemoveAll(dir)

		return "", "", fmt.Errorf("write Claude MCP config: %w", err)
	}

	return path, dir, nil
}
