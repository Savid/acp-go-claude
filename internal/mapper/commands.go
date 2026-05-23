package mapper

import (
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

var unsupportedSlashCommands = map[string]struct{}{
	"cost":             {},
	"keybindings-help": {},
	"login":            {},
	"logout":           {},
	"output-style:new": {},
	"release-notes":    {},
	keyTodos:           {},
}

// AvailableCommandsUpdate converts Claude slash commands into an ACP update.
func AvailableCommandsUpdate(commands []claude.SlashCommand) []acp.SessionUpdate {
	if len(commands) == 0 {
		return nil
	}

	available := make([]acp.AvailableCommand, 0, len(commands))
	for _, command := range commands {
		name := slashCommandName(command.Name)
		if name == "" || unsupportedSlashCommand(name) {
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
	name = strings.TrimSpace(name)

	if withoutSuffix, ok := strings.CutSuffix(name, " (MCP)"); ok {
		name = "mcp:" + withoutSuffix
	}

	if unsafeSlashCommandName(name) {
		return ""
	}

	return name
}

func unsafeSlashCommandName(name string) bool {
	return strings.ContainsAny(name, "\t\r\n")
}

func unsupportedSlashCommand(name string) bool {
	_, ok := unsupportedSlashCommands[name]

	return ok
}
