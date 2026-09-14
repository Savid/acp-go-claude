package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

const EnvConfigDir = "CLAUDE_CONFIG_DIR"
const InternalEnvPrefix = "ACP_GO_CLAUDE_INTERNAL_"

type Launch struct {
	SessionID             string
	Resume                bool
	Model                 string
	PermissionMode        string
	SystemPrompt          string
	Bare                  bool
	OutputSchema          map[string]any
	SettingSources        []string
	SettingsFile          string
	AdditionalDirectories []string
}

func (l Launch) Args() []string {
	args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--include-hook-events", "--permission-prompt-tool", "stdio"}
	if l.Resume {
		args = append(args, "--resume", l.SessionID)
	} else {
		args = append(args, "--session-id", l.SessionID)
	}

	for _, pair := range [][2]string{{"--model", l.Model}, {"--permission-mode", l.PermissionMode}, {"--system-prompt", l.SystemPrompt}, {"--settings", l.SettingsFile}} {
		if pair[1] != "" {
			args = append(args, pair[0], pair[1])
		}
	}

	if l.Bare {
		args = append(args, "--bare")
	}

	if l.SettingSources != nil {
		args = append(args, "--setting-sources="+strings.Join(l.SettingSources, ","))
	}

	if l.OutputSchema != nil {
		data, _ := json.Marshal(l.OutputSchema)
		args = append(args, "--json-schema", string(data))
	}

	for _, dir := range l.AdditionalDirectories {
		args = append(args, "--add-dir", dir)
	}

	return args
}

func ConfigDir(explicit string, lookup func(string) (string, bool)) string {
	if explicit != "" {
		return explicit
	}

	if value, ok := lookup(EnvConfigDir); ok && value != "" {
		return value
	}

	home, _ := lookup("HOME")

	return filepath.Join(home, ".claude")
}
