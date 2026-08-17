package claudeacp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestWriteSessionMCPConfig(t *testing.T) {
	path, dir, err := writeSessionMCPConfig(t.TempDir(), "")
	require.NoError(t, err)
	require.Empty(t, path)
	require.Empty(t, dir)

	path, dir, err = writeSessionMCPConfig(t.TempDir(), `{"mcpServers":{}}`)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.Equal(t, filepath.Join(dir, "mcp.json"), path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.JSONEq(t, `{"mcpServers":{}}`, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	fileParent := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(fileParent, []byte("x"), 0o600))
	_, _, err = writeSessionMCPConfig(fileParent, "config")
	require.Error(t, err)

	mkdirErr := errors.New("mkdir")
	previousMkdir := mcpMkdirTemp
	mcpMkdirTemp = func(string, string) (string, error) { return "", mkdirErr }
	_, _, err = writeSessionMCPConfig(t.TempDir(), "config")
	require.ErrorIs(t, err, mkdirErr)
	mcpMkdirTemp = previousMkdir

	writeErr := errors.New("write")
	removed := ""
	previousWrite, previousRemove := mcpWriteFile, mcpRemoveAll
	mcpWriteFile = func(string, []byte, os.FileMode) error { return writeErr }
	mcpRemoveAll = func(path string) error {
		removed = path

		return nil
	}
	t.Cleanup(func() {
		mcpMkdirTemp = previousMkdir
		mcpWriteFile = previousWrite
		mcpRemoveAll = previousRemove
	})
	_, failedDir, err := writeSessionMCPConfig(t.TempDir(), "config")
	require.ErrorIs(t, err, writeErr)
	require.Empty(t, failedDir)
	require.NotEmpty(t, removed)

	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	_, err = agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMCPServers(
		StdioMCPServer("stdio", "tool", nil, nil),
	)))
	require.ErrorIs(t, err, writeErr)

	previousMapper := mapMCPServersToClaude
	mapMCPServersToClaude = func([]acp.McpServer) (string, error) { return "", writeErr }
	t.Cleanup(func() { mapMCPServersToClaude = previousMapper })
	_, err = agent.mcpConfigForStart(sessionStart{})
	require.ErrorIs(t, err, writeErr)
}
