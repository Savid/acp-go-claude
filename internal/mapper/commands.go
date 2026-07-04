package mapper

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	commandClear               = "clear"
	commandConfig              = "config"
	alternativeSessionNew      = "session/new"
	alternativeSetConfigOption = "session/set_config_option"
)

// goal left suppression on 2026-07-05: TestClaudeGoalCommandLiveProbe proved
// the full /goal loop emits a single terminal result on Claude Code 2.1.200.
var suppressedCommands = map[string]struct{}{
	"cost":             {},
	"heapdump":         {},
	"keybindings-help": {},
	"login":            {},
	"logout":           {},
	"output-style:new": {},
	"release-notes":    {},
	keyTodos:           {},
}

var denyInvokeCommands = map[string]string{
	commandClear:  alternativeSessionNew,
	commandConfig: alternativeSetConfigOption,
}

// AvailableCommandsUpdate converts Claude slash commands into an ACP update.
func AvailableCommandsUpdate(commands []claude.SlashCommand) []acp.SessionUpdate {
	if len(commands) == 0 {
		return nil
	}

	available := make([]acp.AvailableCommand, 0, len(commands))
	for _, command := range commands {
		name := slashCommandName(command.Name)
		if name == "" || suppressedSlashCommand(name) || denyInvokeCommand(name) {
			continue
		}

		availableCommand := acp.AvailableCommand{
			Name:        name,
			Description: command.Description,
		}
		if command.ArgumentHint != "" {
			availableCommand.Input = &acp.AvailableCommandInput{
				Unstructured: &acp.UnstructuredCommandInput{Hint: command.ArgumentHint},
			}
		}

		available = append(available, availableCommand)
	}

	if len(available) == 0 {
		return nil
	}

	return []acp.SessionUpdate{
		{
			AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
				AvailableCommands: available,
			},
		},
	}
}

func slashCommandName(name string) string {
	if withoutSuffix, ok := strings.CutSuffix(name, " (MCP)"); ok {
		name = "mcp:" + withoutSuffix
	}

	if !validSlashCommandName(name) {
		return ""
	}

	return name
}

func validSlashCommandName(name string) bool {
	if name == "" || strings.Contains(name, "/") || !utf8.ValidString(name) {
		return false
	}

	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}

	return true
}

// DeniedPromptCommand returns the supported ACP alternative for a prompt that
// invokes a wrapper-invariant-breaking native command.
func DeniedPromptCommand(prompt []acp.ContentBlock) (string, string, bool) {
	name := PromptCommandName(prompt)
	if name == "" {
		return "", "", false
	}

	alternative, ok := denyInvokeCommands[name]
	if !ok {
		return "", "", false
	}

	return name, alternative, ok
}

// PromptCommandName returns the sanitized leading slash command in the first
// text block, or empty when the prompt is ordinary text.
func PromptCommandName(prompt []acp.ContentBlock) string {
	for _, block := range prompt {
		if block.Text == nil {
			continue
		}

		return leadingSlashCommandName(block.Text.Text)
	}

	return ""
}

func leadingSlashCommandName(text string) string {
	if text == "" || text[0] != '/' {
		return ""
	}

	token := text[1:]
	for index, r := range token {
		if unicode.IsSpace(r) {
			token = token[:index]

			break
		}
	}

	if !validSlashCommandName(token) {
		return ""
	}

	return token
}

func suppressedSlashCommand(name string) bool {
	_, ok := suppressedCommands[name]

	return ok
}

func denyInvokeCommand(name string) bool {
	_, ok := denyInvokeCommands[name]

	return ok
}
