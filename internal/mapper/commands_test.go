package mapper

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAvailableCommandsUpdate(t *testing.T) {
	t.Parallel()

	require.Nil(t, AvailableCommandsUpdate(nil))
	require.Nil(t, AvailableCommandsUpdate([]claude.SlashCommand{{Description: "missing name"}}))

	updates := AvailableCommandsUpdate([]claude.SlashCommand{
		{Name: "debug", Description: "Debug session", ArgumentHint: "[issue]"},
		{Name: "compact"},
		{Name: "server:command (MCP)", Description: "Run MCP command"},
		{Name: "login", Description: "unsupported"},
		{Name: "bad\ncommand", Description: "invalid"},
		{Name: "bad\tcommand (MCP)", Description: "invalid after rewrite"},
	})

	require.Len(t, updates, 1)
	commands := updates[0].AvailableCommandsUpdate.AvailableCommands
	require.Len(t, commands, 3)
	require.Equal(t, "debug", commands[0].Name)
	require.Equal(t, "Debug session", commands[0].Description)
	require.Equal(t, "[issue]", commands[0].Input.Unstructured.Hint)
	require.Empty(t, commands[1].Description)
	require.Nil(t, commands[1].Input)
	require.Equal(t, "mcp:server:command", commands[2].Name)
}
