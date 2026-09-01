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
	"sync"
	"time"
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
	terminal := false

	if options.Authority != nil && !options.TreePrepared && options.ClaudeHome != "" {
		if options.Authority.PrepareNativeTree == nil {
			return nil, NativeResult{}, authorityUnavailable(options.Authority)
		}

		if err := options.Authority.PrepareNativeTree(ctx, options.ClaudeHome); err != nil {
			if errors.Is(err, options.Authority.Unavailable) || errors.Is(err, options.Authority.ContainmentIncomplete) {
				return nil, NativeResult{}, err
			}

			return nil, NativeResult{}, containmentIncomplete(options, "prepare native output tree", err)
		}

		prepared = true
	}

	defer func() {
		if prepared && terminal {
			if reclaimErr := reclaimNativeTree(options.Authority, options.ClaudeHome); reclaimErr != nil {
				returnErr = errors.Join(returnErr, reclaimErr)
			}
		}
	}()

	process, err := startNative(ctx, options, executable, arguments)
	if err != nil {
		// A normal StartNative refusal proves that no child remains, so a tree
		// prepared solely for this command can be reclaimed. Explicit authority
		// loss or containment ambiguity does not authorize path access.
		if prepared {
			terminal = !errors.Is(err, options.Authority.Unavailable) &&
				!errors.Is(err, options.Authority.ContainmentIncomplete)
		}

		return nil, NativeResult{}, err
	}

	stdin := process.Stdin()
	stdout := process.Stdout()
	stderr := process.Stderr()
	stdinErr := stdin.Close()

	stdoutResult := make(chan nativeOutputRead, 1)
	stderrDone := make(chan error, 1)

	go func() {
		var (
			bounded boundedNativeOutput
			readErr error
		)

		defer func() {
			if recover() != nil {
				readErr = errors.Join(readErr, errClaudeStdoutReaderPanic)
			}

			stdoutResult <- nativeOutputRead{data: bounded.Bytes(), err: errors.Join(readErr, bounded.err)}
		}()

		_, readErr = io.Copy(&bounded, stdout)
	}()

	go func() {
		var readErr error

		defer func() {
			if recover() != nil {
				readErr = errors.Join(readErr, errClaudeTransportFailure)
			}

			stderrDone <- readErr
		}()

		_, readErr = io.Copy(io.Discard, stderr)
	}()

	var (
		terminalResult  NativeResult
		terminalWaitErr error
	)

	waitDone := make(chan struct{})

	// #nosec G118 -- the authority owns process settlement after a caller detaches.
	go func() {
		terminalResult, terminalWaitErr = process.Wait(context.Background())

		close(waitDone)
	}()

	var waitErr error

	select {
	case <-waitDone:
		result = terminalResult
		waitErr = terminalWaitErr
	case <-ctx.Done():
		waitErr = ctx.Err()
	}

	if waitErr != nil {
		revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), processShutdownWaitDelay)
		revokeErr := process.Revoke(revokeCtx)

		cancelRevoke()

		terminalCtx, cancelTerminal := context.WithTimeout(context.Background(), processShutdownWaitDelay)
		select {
		case <-waitDone:
			result = terminalResult

			switch {
			case terminalWaitErr == nil:
				terminal = true
			case options.Authority != nil:
				waitErr = errors.Join(waitErr, containmentIncomplete(options, "wait for native output process", terminalWaitErr))
			default:
				waitErr = errors.Join(waitErr, terminalWaitErr)
			}
		case <-terminalCtx.Done():
			waitErr = errors.Join(waitErr, containmentIncomplete(options, "wait for native output process", terminalCtx.Err()))
		}

		cancelTerminal()

		waitErr = errors.Join(waitErr, revokeErr)
	} else {
		terminal = true
	}

	read := awaitNativeOutput(stdoutResult, stdout)
	stderrErr := awaitNativeDrain(stderrDone, stderr)

	return read.data, result, errors.Join(stdinErr, waitErr, read.err, stderrErr)
}

const nativeOutputMaxBytes = 10 * 1024 * 1024

var nativeOutputDrainDelay = 250 * time.Millisecond

type nativeOutputRead struct {
	data []byte
	err  error
}

type boundedNativeOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
	err error
}

func (w *boundedNativeOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := nativeOutputMaxBytes - w.buf.Len()
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}

		_, _ = w.buf.Write(data[:remaining])
	}

	if len(data) > remaining && w.err == nil {
		w.err = fmt.Errorf("native output exceeds %d bytes", nativeOutputMaxBytes)
	}

	return len(data), nil
}

func (w *boundedNativeOutput) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]byte(nil), w.buf.Bytes()...)
}

func awaitNativeOutput(done <-chan nativeOutputRead, stream io.Closer) nativeOutputRead {
	select {
	case result := <-done:
		return result
	case <-time.After(nativeOutputDrainDelay):
		closeErr := stream.Close()

		select {
		case result := <-done:
			result.err = errors.Join(result.err, closeErr, errors.New("native stdout remained open after terminal wait"))

			return result
		case <-time.After(nativeOutputDrainDelay):
			return nativeOutputRead{err: errors.Join(closeErr, errors.New("native stdout did not close after terminal wait"))}
		}
	}
}

func awaitNativeDrain(done <-chan error, stream io.Closer) error {
	select {
	case err := <-done:
		return err
	case <-time.After(nativeOutputDrainDelay):
		closeErr := stream.Close()

		select {
		case err := <-done:
			return errors.Join(err, closeErr, errors.New("native stderr remained open after terminal wait"))
		case <-time.After(nativeOutputDrainDelay):
			return errors.Join(closeErr, errors.New("native stderr did not close after terminal wait"))
		}
	}
}
