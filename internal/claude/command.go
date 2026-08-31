package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	envClaudeCodeNested     = "CLAUDECODE"
	envSearchPath           = "PATH"
	envClaudeConfigDir      = "CLAUDE_CONFIG_DIR"
	envHome                 = "HOME"
	cliArgOutputFormat      = "--output-format"
	defaultCLIExecutable    = "claude"
	minClaudeVersion        = "2.0.0"
	privateAdapterEnvPrefix = "ACP_GO_CLAUDE_INTERNAL_"
)

func BuildArgs(options Options) []string {
	args := []string{cliArgOutputFormat, streamJSON, "--input-format", streamJSON, "--include-partial-messages", "--verbose", "--include-hook-events"}
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

	if options.MCPConfigPath != "" {
		args = append(args, "--mcp-config", options.MCPConfigPath, "--strict-mcp-config")
	}

	if options.SettingSources != nil {
		args = append(args, "--setting-sources="+strings.Join(options.SettingSources, ","))
	}

	if options.SettingsFile != "" {
		args = append(args, "--settings", options.SettingsFile)
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

func BuildEnv(options Options) []string {
	base := options.OrdinaryEnvironment
	if options.Authority != nil {
		if options.Authority.NativeEnvironment == nil {
			return nil
		}

		base = options.Authority.NativeEnvironment()
	}

	if validateEnvironmentMap(base) != nil {
		return nil
	}

	values := make(map[string]string, len(base)+len(options.Env)+4)
	set := func(key, value string) {
		if EnvironmentKey(key) == EnvironmentKey(envClaudeCodeNested) || privateAdapterEnvName(key) || authScrubbedEnvKey(key) {
			return
		}

		values[EnvironmentKey(key)] = value
	}

	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		if EnvironmentKey(key) != EnvironmentKey(envClaudeConfigDir) {
			set(key, base[key])
		}
	}

	set("CLAUDE_CODE_ENTRYPOINT", "acp-go-claude")

	keys = keys[:0]
	for key := range options.Env {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		if !managedRootEnvKey(key) {
			set(key, options.Env[key])
		}
	}

	if options.ClaudeHome != "" {
		set(envClaudeConfigDir, options.ClaudeHome)
	}

	if options.Cwd != "" {
		set("PWD", options.Cwd)
	}

	if len(options.ExtraPathDirs) > 0 {
		set(envSearchPath, prependSearchPath(options.ExtraPathDirs, values[EnvironmentKey(envSearchPath)]))
	}

	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}

	return environment
}

func validateEnvironmentMap(environment map[string]string) error {
	if environment == nil {
		return errors.New("native environment is required")
	}

	for key, value := range environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry for %q", key)
		}
	}

	return nil
}

func privateAdapterEnvName(key string) bool {
	return strings.HasPrefix(EnvironmentKey(key), privateAdapterEnvPrefix)
}

func managedRootEnvKey(key string) bool {
	switch EnvironmentKey(key) {
	case envClaudeConfigDir, envHome, "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}

func prependSearchPath(dirs []string, search string) string {
	entries := append([]string(nil), dirs...)
	if search != "" {
		entries = append(entries, search)
	}

	return strings.Join(entries, string(os.PathListSeparator))
}

func validateClaudeVersion(ctx context.Context, options Options) error {
	output, result, err := runNativeOutput(ctx, options, options.CLIPath, []string{"--version"})
	if err != nil {
		return fmt.Errorf("probe claude version: %w", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("probe claude version exited %d", result.ExitCode)
	}

	version := parseClaudeVersion(string(output))
	if version == "" {
		return errors.New("parse claude version")
	}

	if compareSemver(version, minClaudeVersion) < 0 {
		return fmt.Errorf("claude version %s is older than required %s", version, minClaudeVersion)
	}

	return nil
}

var claudeVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

func parseClaudeVersion(output string) string {
	match := claudeVersionRE.FindString(output)
	if match == "" {
		return ""
	}

	if cut, _, ok := strings.Cut(match, "-"); ok {
		match = cut
	}

	if cut, _, ok := strings.Cut(match, "+"); ok {
		match = cut
	}

	return match
}

func compareSemver(left, right string) int {
	leftParts, rightParts := semverParts(left), semverParts(right)
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}

		if leftParts[index] > rightParts[index] {
			return 1
		}
	}

	return 0
}

func semverParts(value string) [3]int {
	var out [3]int

	parts := strings.Split(value, ".")
	for index := 0; index < len(out) && index < len(parts); index++ {
		out[index], _ = strconv.Atoi(parts[index])
	}

	return out
}

func runNativeOutput(ctx context.Context, options Options, executable string, arguments []string) (output []byte, result NativeResult, returnErr error) {
	prepared := false

	if options.Authority != nil && !options.TreePrepared && options.ClaudeHome != "" {
		if options.Authority.PrepareNativeTree == nil {
			return nil, NativeResult{}, authorityUnavailable(options.Authority)
		}

		if err := options.Authority.PrepareNativeTree(ctx, options.ClaudeHome); err != nil {
			return nil, NativeResult{}, err
		}

		prepared = true
	}

	defer func() {
		if prepared {
			returnErr = errors.Join(returnErr, reclaimNativeTree(options.Authority, options.ClaudeHome))
		}
	}()

	process, err := startNative(ctx, options, executable, arguments)
	if err != nil {
		return nil, NativeResult{}, err
	}

	_ = process.Stdin().Close()

	var stderr bytes.Buffer

	stderrDone := make(chan struct{})

	go func() { _, _ = io.Copy(&stderr, process.Stderr()); close(stderrDone) }()

	var readErr error

	output, readErr = io.ReadAll(process.Stdout())

	result, waitErr := process.Wait(ctx)
	if ctx.Err() != nil {
		revokeErr := process.Revoke(context.Background())
		result, waitErr = process.Wait(context.Background())
		waitErr = errors.Join(ctx.Err(), revokeErr, waitErr)
	}

	<-stderrDone

	if readErr != nil {
		waitErr = errors.Join(waitErr, readErr)
	}

	return output, result, waitErr
}
