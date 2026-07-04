package mapper

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAvailableCommandsUpdate(t *testing.T) {
	t.Parallel()

	require.Nil(t, AvailableCommandsUpdate(nil))
	require.Nil(t, AvailableCommandsUpdate([]claude.SlashCommand{{Description: "missing name"}}))

	updates := AvailableCommandsUpdate([]claude.SlashCommand{
		{Name: "debug", Description: "Debug session", ArgumentHint: "[issue]", Aliases: []string{"dbg"}},
		{Name: "compact"},
		{Name: "mcp:server:command", Description: "Run namespaced command"},
		{Name: "server:command (MCP)", Description: "Run MCP command"},
		{Name: "cost", Description: "suppressed"},
		{Name: "heapdump", Description: "suppressed"},
		{Name: commandClear, Description: "denied", Aliases: []string{"reset", "new"}},
		{Name: commandConfig, Description: "denied", Aliases: []string{"settings"}},
		{Name: "login", Description: "unsupported"},
		{Name: "bad\ncommand", Description: "invalid"},
		{Name: "bad\tcommand (MCP)", Description: "invalid after rewrite"},
	})

	require.Len(t, updates, 1)
	commands := updates[0].AvailableCommandsUpdate.AvailableCommands
	require.Len(t, commands, 4)
	require.Equal(t, "debug", commands[0].Name)
	require.Equal(t, "Debug session", commands[0].Description)
	require.Equal(t, "[issue]", commands[0].Input.Unstructured.Hint)
	require.Empty(t, commands[1].Description)
	require.Nil(t, commands[1].Input)
	require.Equal(t, "mcp:server:command", commands[2].Name)
	require.Equal(t, "mcp:server:command", commands[3].Name)
	require.NotContains(t, []string{commands[0].Name, commands[1].Name, commands[2].Name, commands[3].Name}, "dbg")
	require.NotContains(t, []string{commands[0].Name, commands[1].Name, commands[2].Name, commands[3].Name}, "settings")
	require.NotContains(t, []string{commands[0].Name, commands[1].Name, commands[2].Name, commands[3].Name}, "reset")
	require.NotContains(t, []string{commands[0].Name, commands[1].Name, commands[2].Name, commands[3].Name}, "new")
}

func TestAvailableCommandsUpdateSanitizerGrammar(t *testing.T) {
	t.Parallel()

	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "empty", command: "", want: false},
		{name: "slash", command: "bad/name", want: false},
		{name: "invalid utf8", command: invalidUTF8, want: false},
		{name: "ascii whitespace", command: "bad command", want: false},
		{name: "unicode whitespace", command: "bad\u00a0command", want: false},
		{name: "control", command: "bad\u0000command", want: false},
		{name: "format", command: "bad\u200bcommand", want: false},
		{name: "colon namespace", command: "mcp:server:command", want: true},
		{name: "slash path with colon", command: "apps/web:deploy", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			updates := AvailableCommandsUpdate([]claude.SlashCommand{{Name: tc.command}})
			if !tc.want {
				require.Nil(t, updates)

				return
			}

			require.Len(t, updates, 1)
			require.Equal(t, tc.command, updates[0].AvailableCommandsUpdate.AvailableCommands[0].Name)
		})
	}
}

func TestGoalCommandIsAdvertised(t *testing.T) {
	t.Parallel()

	// The 2026-07-05 probe pass on Claude Code 2.1.200 and
	// decisions/15-sc-iter1-adjudication.md require goal to stay advertised.
	require.False(t, suppressedSlashCommand("goal"))

	updates := AvailableCommandsUpdate([]claude.SlashCommand{{Name: "goal", Description: "Set a goal"}})

	require.Len(t, updates, 1)
	require.Equal(t, "goal", updates[0].AvailableCommandsUpdate.AvailableCommands[0].Name)
}

func TestDeniedPromptCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prompt     []acp.ContentBlock
		wantName   string
		wantAlt    string
		wantDenied bool
	}{
		{
			name:       "clear",
			prompt:     []acp.ContentBlock{acp.TextBlock("/" + commandClear + " now")},
			wantName:   commandClear,
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "config",
			prompt:     []acp.ContentBlock{acp.TextBlock("/" + commandConfig + " model=sonnet")},
			wantName:   commandConfig,
			wantAlt:    alternativeSetConfigOption,
			wantDenied: true,
		},
		{
			name:       "tab delimiter",
			prompt:     []acp.ContentBlock{acp.TextBlock("/" + commandClear + "\tplease")},
			wantName:   commandClear,
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "unicode delimiter",
			prompt:     []acp.ContentBlock{acp.TextBlock("/" + commandClear + "\u00a0please")},
			wantName:   commandClear,
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "leading whitespace bypasses",
			prompt:     []acp.ContentBlock{acp.TextBlock(" /" + commandClear)},
			wantDenied: false,
		},
		{
			name:       "exact match only",
			prompt:     []acp.ContentBlock{acp.TextBlock("/clearly")},
			wantDenied: false,
		},
		{
			name:       "invalid sanitized token bypasses",
			prompt:     []acp.ContentBlock{acp.TextBlock("/" + commandClear + "\u200b")},
			wantDenied: false,
		},
		{
			name:       "first text block only",
			prompt:     []acp.ContentBlock{acp.ImageBlock("abc", "image/png"), acp.TextBlock("plain"), acp.TextBlock("/" + commandClear)},
			wantDenied: false,
		},
		{
			name:       "non-first content before first text allowed",
			prompt:     []acp.ContentBlock{acp.ImageBlock("abc", "image/png"), acp.TextBlock("/" + commandClear)},
			wantName:   commandClear,
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "no text block",
			prompt:     []acp.ContentBlock{acp.ImageBlock("abc", "image/png")},
			wantDenied: false,
		},
		{
			name:       "empty first text block",
			prompt:     []acp.ContentBlock{acp.TextBlock(""), acp.TextBlock("/" + commandClear)},
			wantDenied: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotName, gotAlt, gotDenied := DeniedPromptCommand(tc.prompt)
			require.Equal(t, tc.wantDenied, gotDenied)
			require.Equal(t, tc.wantName, gotName)
			require.Equal(t, tc.wantAlt, gotAlt)
		})
	}
}

func TestDeniedPromptCommandNativeAliases(t *testing.T) {
	t.Parallel()

	commands := []claude.SlashCommand{
		{Name: commandConfig, Aliases: []string{"settings", "bad alias"}},
		{Name: commandClear, Aliases: []string{"reset", "new", "bad/name"}},
		{Name: "help", Aliases: []string{"config-help"}},
	}

	tests := []struct {
		name       string
		prompt     []acp.ContentBlock
		wantName   string
		wantAlt    string
		wantDenied bool
	}{
		{
			name:       "config alias",
			prompt:     []acp.ContentBlock{acp.TextBlock("/settings model=sonnet")},
			wantName:   "settings",
			wantAlt:    alternativeSetConfigOption,
			wantDenied: true,
		},
		{
			name:       "clear reset alias",
			prompt:     []acp.ContentBlock{acp.TextBlock("/reset now")},
			wantName:   "reset",
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "clear new alias",
			prompt:     []acp.ContentBlock{acp.TextBlock("/new")},
			wantName:   "new",
			wantAlt:    alternativeSessionNew,
			wantDenied: true,
		},
		{
			name:       "leading whitespace alias bypasses",
			prompt:     []acp.ContentBlock{acp.TextBlock(" /reset")},
			wantDenied: false,
		},
		{
			name:       "non-deny command alias ignored",
			prompt:     []acp.ContentBlock{acp.TextBlock("/config-help")},
			wantDenied: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotName, gotAlt, gotDenied := DeniedPromptCommand(tc.prompt, commands)
			require.Equal(t, tc.wantDenied, gotDenied)
			require.Equal(t, tc.wantName, gotName)
			require.Equal(t, tc.wantAlt, gotAlt)
		})
	}
}
