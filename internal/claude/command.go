package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const envClaudeCodeNested = "CLAUDECODE"

var commandEnviron = os.Environ

// BuildArgs returns the Claude CLI arguments for ACP-backed interactive sessions.
func BuildArgs(options Options) []string {
	args := []string{
		"--output-format", streamJSON,
		"--input-format", streamJSON,
		"--include-partial-messages",
		"--verbose",
		"--include-hook-events",
	}

	if options.SessionMirror {
		args = append(args, "--session-mirror")
	}

	if options.Bare {
		args = append(args, "--bare")
	}

	if options.PermissionMode != "" {
		args = append(args, "--permission-mode", options.PermissionMode)
	}

	if options.AllowSkipPermissionsArg && options.PermissionMode != "bypassPermissions" {
		args = append(args, "--allow-dangerously-skip-permissions")
	}

	if options.PermissionPromptTool != "" {
		args = append(args, "--permission-prompt-tool", options.PermissionPromptTool)
	}

	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}

	if options.SystemText != "" {
		args = append(args, "--system-prompt", options.SystemText)
	}

	if len(options.JSONSchema) > 0 {
		args = append(args, "--json-schema", compactJSON(options.JSONSchema))
	}

	if options.MCPConfigJSON != "" {
		args = append(args, "--mcp-config", options.MCPConfigJSON, "--strict-mcp-config")
	}

	if options.SettingSources != nil {
		args = append(args, "--setting-sources="+strings.Join(options.SettingSources, ","))
	}

	for _, dir := range options.AddDirs {
		if strings.TrimSpace(dir) != "" {
			args = append(args, "--add-dir", dir)
		}
	}

	if options.ResumeID != "" {
		args = append(args, "--resume", options.ResumeID)
		if options.ForkSession {
			args = append(args, "--fork-session")
			if options.SessionID != "" {
				args = append(args, "--session-id", options.SessionID)
			}
		}
	} else if options.SessionID != "" {
		args = append(args, "--session-id", options.SessionID)
	}

	return args
}

func compactJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}

	return string(data)
}

// BuildEnv returns the environment for a Claude CLI process.
func BuildEnv(options Options) []string {
	values := make(map[string]string)
	keys := make([]string, 0, len(os.Environ())+len(options.Env)+3)

	set := func(key string, value string) {
		if key == envClaudeCodeNested {
			return
		}

		if _, ok := values[key]; !ok {
			keys = append(keys, key)
		}

		values[key] = value
	}

	for _, item := range commandEnviron() {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}

		set(key, value)
	}

	if options.ClaudeHome != "" {
		set("CLAUDE_CONFIG_DIR", options.ClaudeHome)
	}

	set("CLAUDE_CODE_ENTRYPOINT", "acp-go-claude")

	optionKeys := make([]string, 0, len(options.Env))
	for key := range options.Env {
		optionKeys = append(optionKeys, key)
	}

	slices.Sort(optionKeys)

	for _, key := range optionKeys {
		set(key, options.Env[key])
	}

	if options.Cwd != "" {
		set("PWD", options.Cwd)
	}

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

// Discover finds the Claude executable.
func Discover(ctx context.Context, cliPath string, _ map[string]string) (string, error) {
	if strings.TrimSpace(cliPath) != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		return cliPath, nil
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("find claude in PATH: %w", err)
	}

	return path, nil
}
