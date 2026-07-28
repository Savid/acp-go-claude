package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const envClaudeCodeNested = "CLAUDECODE"
const cliArgOutputFormat = "--output-format"

// minClaudeVersion is the oldest Claude CLI the adapter supports. The adapter's
// stream-json control protocol (bidirectional control requests, partial-message
// streaming, session mirror, hook events) requires the Claude Code 2.x line.
const minClaudeVersion = "2.0.0"

var commandEnviron = os.Environ

// BuildArgs returns the Claude CLI arguments for ACP-backed interactive sessions.
func BuildArgs(options Options) []string {
	args := []string{
		cliArgOutputFormat, streamJSON,
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

// BuildEnv returns the environment for a Claude CLI process. The scrubbed
// variables are dropped here rather than at one spawn site, because a value
// that repoints the credential store or rewrites the child's own output bytes
// is inherited by every child alike: a login writing the default store while
// the residence probe describes a different one reports success about a store
// nobody asked about.
func BuildEnv(options Options) []string {
	values := make(map[string]string)
	keys := make([]string, 0, len(os.Environ())+len(options.Env)+3)

	set := func(key string, value string) {
		if key == envClaudeCodeNested || strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
			return
		}

		if authScrubbedEnvKey(key) {
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

// validateClaudeVersion probes the Claude CLI version and fails fast when it is
// older than minClaudeVersion. The adapter never silently downgrades to an
// unsupported CLI.
func validateClaudeVersion(ctx context.Context, path string, options Options) error {
	release := func() {}

	if options.AcquireVersionDiscovery != nil {
		acquired, err := options.AcquireVersionDiscovery(ctx)
		if err != nil {
			return fmt.Errorf("admit claude CLI version discovery: %w", err)
		}

		if acquired == nil {
			return errors.New("admit claude CLI version discovery: nil release")
		}

		release = acquired
	}

	output, err := containedClaudeVersionOutput(ctx, path, options)
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		release()
	}

	if err != nil {
		return fmt.Errorf("check claude CLI version: %w", err)
	}

	version := parseClaudeVersion(string(output))
	if version == "" {
		return fmt.Errorf("check claude CLI version: could not parse %q", strings.TrimSpace(string(output)))
	}

	if compareSemver(version, minClaudeVersion) < 0 {
		return fmt.Errorf("claude CLI %s is too old; need >= %s", version, minClaudeVersion)
	}

	return nil
}

func containedClaudeVersionOutput(ctx context.Context, path string, options Options) (output []byte, returnErr error) {
	var (
		generation *DarwinGeneration
		err        error
	)

	if options.DarwinBestEffort {
		if options.PrepareDarwinVersionGeneration == nil {
			return nil, fmt.Errorf("%w: Darwin version discovery generation is unavailable", ErrProcessContainmentIncomplete)
		}

		generation, err = options.PrepareDarwinVersionGeneration(ctx)
		if err != nil {
			return nil, err
		}
	}

	return containedClaudeOutput(ctx, path, []string{"--version"}, options, generation, "claude version")
}

func containedClaudeOutput(
	ctx context.Context,
	path string,
	args []string,
	options Options,
	generation *DarwinGeneration,
	operation string,
) (output []byte, returnErr error) {
	var err error

	generationOwnedByTree := false
	defer func() {
		if generation != nil && !generationOwnedByTree {
			complete := !errors.Is(returnErr, ErrProcessContainmentIncomplete)
			returnErr = errors.Join(returnErr, generation.finish(complete))
		}
	}()

	command := processCommand(path, args...)
	configureProcessCommand(command)
	command.Dir = options.Cwd

	if command.Dir == "" {
		command.Dir, err = processGetwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory for %s: %w", operation, err)
		}
	}

	envOptions := options
	envOptions.Cwd = command.Dir
	command.Env = BuildEnv(envOptions)

	launch, err := processPrepareContained(command, processLaunchOptions{
		DarwinBestEffort: options.DarwinBestEffort,
		Generation:       generation,
	})
	if err != nil {
		if errors.Is(err, ErrProcessContainmentIncomplete) && options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return nil, fmt.Errorf("prepare %s containment: %w", operation, err)
	}

	stdout, err := launch.cmd.StdoutPipe()
	if err != nil {
		launch.close()

		return nil, fmt.Errorf("capture %s output: %w", operation, err)
	}

	tree, err := processStartContained(launch)
	if err != nil {
		_ = stdout.Close()

		if errors.Is(err, ErrProcessContainmentIncomplete) && options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return nil, fmt.Errorf("start %s: %w", operation, err)
	}

	generationOwnedByTree = true

	if options.ObserveProcessInventory != nil {
		options.ObserveProcessInventory(ctx, tree.processSnapshot)
	}

	type readResult struct {
		data []byte
		err  error
	}

	readDone := make(chan readResult, 1)

	go func() {
		data, readErr := io.ReadAll(stdout)
		readDone <- readResult{data: data, err: readErr}
	}()

	var (
		contextErr error
		read       readResult
	)

	select {
	case read = <-readDone:
	case <-ctx.Done():
		contextErr = ctx.Err()
		containErr := tree.quiesce(processShutdownWaitDelay)
		waitErr := tree.wait(launch.cmd)
		read = <-readDone
		closeErr := processContainmentClose(tree)

		observeAuxiliaryQuiescence(options, containErr)

		return read.data, errors.Join(contextErr, read.err, waitErr, containErr, closeErr)
	}

	waitErr := tree.wait(launch.cmd)
	containErr := tree.quiesce(processShutdownWaitDelay)
	closeErr := processContainmentClose(tree)

	observeAuxiliaryQuiescence(options, containErr)

	return read.data, errors.Join(read.err, waitErr, containErr, closeErr)
}

func observeAuxiliaryQuiescence(options Options, containmentErr error) {
	if containmentErr != nil {
		if options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(context.Background(), unavailableProcessInventory)
		}

		return
	}

	if options.ObserveProcessQuiesced != nil {
		options.ObserveProcessQuiesced(context.Background())
	}
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

func compareSemver(left string, right string) int {
	leftParts := semverParts(left)
	rightParts := semverParts(right)

	for i := range leftParts {
		switch {
		case leftParts[i] < rightParts[i]:
			return -1
		case leftParts[i] > rightParts[i]:
			return 1
		}
	}

	return 0
}

func semverParts(value string) [3]int {
	var out [3]int

	parts := strings.Split(value, ".")
	for i := range out {
		if i >= len(parts) {
			break
		}

		out[i], _ = strconv.Atoi(parts[i])
	}

	return out
}
