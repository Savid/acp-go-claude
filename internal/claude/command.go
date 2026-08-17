package claude

import (
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

const envClaudeCodeNested = "CLAUDECODE"
const envSearchPath = "PATH"
const envClaudeConfigDir = "CLAUDE_CONFIG_DIR"
const cliArgOutputFormat = "--output-format"

// defaultCLIExecutable is the executable the adapter resolves when no CLI path
// was configured. It is the executable's own identity, which is not the Darwin
// registry vendor that happens to spell the same word.
const defaultCLIExecutable = "claude"

var commandPipe = os.Pipe

// minClaudeVersion is the oldest Claude CLI the adapter supports. The adapter's
// stream-json control protocol (bidirectional control requests, partial-message
// streaming, session mirror, hook events) requires the Claude Code 2.x line.
const minClaudeVersion = "2.0.0"

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
	base, err := launchBaseEnvironment(options)
	if err != nil {
		return nil
	}

	values := make(map[string]string, len(base)+len(options.Env)+3)
	keys := make([]string, 0, len(base)+len(options.Env)+3)

	set := func(key string, value string) {
		if EnvironmentKey(key) == EnvironmentKey(envClaudeCodeNested) || privateAdapterEnvName(key) {
			return
		}

		if authScrubbedEnvKey(key) {
			return
		}

		nativeKey := EnvironmentKey(key)
		if _, ok := values[nativeKey]; !ok {
			keys = append(keys, nativeKey)
		}

		values[nativeKey] = value
	}

	baseKeys := make([]string, 0, len(base))
	for key := range base {
		baseKeys = append(baseKeys, key)
	}

	slices.Sort(baseKeys)

	for _, key := range baseKeys {
		if EnvironmentKey(key) == EnvironmentKey(envClaudeConfigDir) {
			continue
		}

		set(key, base[key])
	}

	set("CLAUDE_CODE_ENTRYPOINT", "acp-go-claude")

	optionKeys := make([]string, 0, len(options.Env))
	for key := range options.Env {
		optionKeys = append(optionKeys, key)
	}

	slices.Sort(optionKeys)

	for _, key := range optionKeys {
		if managedRootEnvKey(key) {
			continue
		}

		set(key, options.Env[key])
	}

	if options.ClaudeHome != "" {
		set(envClaudeConfigDir, options.ClaudeHome)
	}

	if options.Cwd != "" {
		set("PWD", options.Cwd)
	}

	if len(options.ExtraPathDirs) > 0 {
		set(envSearchPath, prependSearchPath(
			options.ExtraPathDirs,
			values[EnvironmentKey(envSearchPath)],
		))
	}

	// The absolute-entry rule belongs to the hardened policy PATH. Ordinary
	// execution inherits the operator's own search path and is not held to it.
	if options.ProcessIsolation != nil {
		if err := validateProcessSearchPath(values[EnvironmentKey(envSearchPath)]); err != nil {
			return nil
		}
	}

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

// launchBaseEnvironment answers with the base every native environment is built
// on. An explicit policy supplies a complete replacement base; omission carries
// no policy at all, so the base is the sanitized ambient capture ordinary
// same-identity execution runs with.
func launchBaseEnvironment(options Options) (map[string]string, error) {
	if options.ProcessIsolation == nil {
		if err := validateEnvironmentMap(options.OrdinaryEnvironment); err != nil {
			return nil, fmt.Errorf("validate ordinary launch environment: %w", err)
		}

		return options.OrdinaryEnvironment, nil
	}

	if err := validateProcessIsolation(options.ProcessIsolation); err != nil {
		return nil, err
	}

	return options.ProcessIsolation.BaseEnvironment, nil
}

func managedRootEnvKey(key string) bool {
	switch EnvironmentKey(key) {
	case envClaudeConfigDir, "HOME",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}

// prependSearchPath returns a PATH value carrying dirs ahead of every entry
// already in search, in the order given. Callers own absoluteness: a relative
// entry here would resolve against the child's working directory.
func prependSearchPath(dirs []string, search string) string {
	entries := make([]string, 0, len(dirs)+1)
	entries = append(entries, dirs...)

	if search != "" {
		entries = append(entries, search)
	}

	return strings.Join(entries, string(os.PathListSeparator))
}

// Discover admits the Claude executable exactly once and freezes what it found.
// The result is an identity, not a name to look up again: every exec that
// follows re-reads the admitted path and refuses when the file underneath it
// changed.
func Discover(ctx context.Context, cliPath string, options Options) (Executable, error) {
	if err := ctx.Err(); err != nil {
		return Executable{}, err
	}

	if _, err := launchBaseEnvironment(options); err != nil {
		return Executable{}, err
	}

	if strings.TrimSpace(cliPath) == "" {
		cliPath = defaultCLIExecutable
	}

	path, err := resolveLaunchExecutable(options, cliPath, BuildEnv(options))
	if err != nil {
		return Executable{}, fmt.Errorf("find claude in PATH: %w", err)
	}

	return freezeExecutable(path)
}

// validateClaudeVersion probes the Claude CLI version and fails fast when it is
// older than minClaudeVersion. The adapter never silently downgrades to an
// unsupported CLI.
func validateClaudeVersion(ctx context.Context, executable Executable, options Options) error {
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

	output, err := containedClaudeVersionOutput(ctx, executable, options)
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

func containedClaudeVersionOutput(ctx context.Context, executable Executable, options Options) (output []byte, returnErr error) {
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

	return containedClaudeOutput(ctx, executable, []string{"--version"}, options, generation, "claude version")
}

func containedClaudeOutput(
	ctx context.Context,
	executable Executable,
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

	if verifyErr := executable.verify(); verifyErr != nil {
		return nil, fmt.Errorf("admit %s executable: %w", operation, verifyErr)
	}

	command := processCommand(executable.Path(), args...)
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
	if command.Env == nil {
		return nil, errors.New("build Claude process environment: invalid process isolation")
	}

	stdout, childStdout, err := commandPipe()
	if err != nil {
		return nil, fmt.Errorf("capture %s output: %w", operation, err)
	}

	command.Stdout = childStdout

	launch, err := processPrepareContained(command, processLaunchOptions{
		DarwinBestEffort: options.DarwinBestEffort,
		Generation:       generation,
		Isolation:        options.ProcessIsolation,
	})
	if err != nil {
		closeErr := errors.Join(stdout.Close(), childStdout.Close())

		if errors.Is(err, ErrProcessContainmentIncomplete) && options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return nil, errors.Join(fmt.Errorf("prepare %s containment: %w", operation, err), closeErr)
	}

	if launch.cmd.Stdout != childStdout {
		launch.close()

		closeErr := errors.Join(stdout.Close(), childStdout.Close())

		return nil, errors.Join(
			errors.New("capture "+operation+" output: containment replaced stdout"),
			closeErr,
		)
	}

	tree, err := processStartContained(launch)
	if err != nil {
		closeErr := errors.Join(stdout.Close(), childStdout.Close())

		if errors.Is(err, ErrProcessContainmentIncomplete) && options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return nil, errors.Join(fmt.Errorf("start %s: %w", operation, err), closeErr)
	}

	generationOwnedByTree = true

	childStdoutCloseErr := childStdout.Close()

	stdoutClosed := false
	defer func() {
		if !stdoutClosed {
			returnErr = errors.Join(returnErr, stdout.Close())
		}
	}()

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
		stdoutCloseErr := stdout.Close()
		stdoutClosed = true
		containErr := processBoundaryComplete(tree, processShutdownWaitDelay)
		waitErr := processWaitContained(tree, launch.cmd)
		read = <-readDone
		closeErr := processContainmentClose(tree)

		observeAuxiliaryBoundaryComplete(options, containErr)

		return read.data, errors.Join(
			contextErr, read.err, waitErr, containErr, closeErr, stdoutCloseErr, childStdoutCloseErr,
		)
	}

	waitErr := processWaitContained(tree, launch.cmd)
	containErr := processBoundaryComplete(tree, processShutdownWaitDelay)
	closeErr := processContainmentClose(tree)

	observeAuxiliaryBoundaryComplete(options, containErr)

	return read.data, errors.Join(read.err, waitErr, containErr, closeErr, childStdoutCloseErr)
}

// observeAuxiliaryBoundaryComplete reports how an auxiliary launch's boundary
// ended. The boundary may be the ordinary one, so this says only that it
// completed; a whole-tree claim belongs to the boundaries that can prove one.
func observeAuxiliaryBoundaryComplete(options Options, containmentErr error) {
	if containmentErr != nil {
		if options.ObserveProcessInventory != nil {
			options.ObserveProcessInventory(context.Background(), unavailableProcessInventory)
		}

		return
	}

	if options.ObserveBoundaryComplete != nil {
		options.ObserveBoundaryComplete(context.Background())
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
