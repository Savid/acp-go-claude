package claude

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	privateAdapterEnvPrefix = "ACP_" + "GO_CLAUDE_INTERNAL_"
	DarwinRuntimeIDEnv      = "ACP_GO_CLAUDE_RUNTIME_ID"
	DarwinScratchRootEnv    = "ACP_GO_CLAUDE_SCRATCH_ROOT"
	defaultCloseWait        = 5 * time.Second
	defaultCloseKillAfter   = 500 * time.Millisecond
)

// DarwinGeneration carries the wrapper-owned identity and scratch root for
// one native launch. Its completion and release are memoized together.
type DarwinGeneration struct {
	RuntimeID      string
	ScratchRoot    string
	RecordStarted  func(pid, pgid int) error
	RecordFinished func(complete bool) error
	Release        func(complete bool) error

	finishOnce sync.Once
	finishErr  error
}

func (generation *DarwinGeneration) prepareCommand(command *exec.Cmd) error {
	if generation == nil || command == nil {
		return errors.New("darwin containment generation is unavailable")
	}

	temporaryRoot := filepath.Join(generation.ScratchRoot, "tmp")
	if err := os.MkdirAll(temporaryRoot, 0o700); err != nil {
		return fmt.Errorf("create Darwin command temporary root: %w", err)
	}

	for index := 0; index+1 < len(command.Args); index++ {
		var destination string

		switch command.Args[index] {
		case "--mcp-config":
			destination = filepath.Join(generation.ScratchRoot, "mcp.json")
		case "--settings":
			destination = filepath.Join(generation.ScratchRoot, "settings.json")
		default:
			continue
		}

		// #nosec G703 -- the source path is a validated wrapper-generated command input.
		contents, err := os.ReadFile(command.Args[index+1])
		if err != nil {
			return fmt.Errorf("read Darwin command input: %w", err)
		}
		// #nosec G703 -- destination is wrapper-constructed inside the mode-0700 generation root.
		if err := os.WriteFile(destination, contents, 0o600); err != nil {
			return fmt.Errorf("write Darwin command input: %w", err)
		}

		command.Args[index+1] = destination
	}

	overrides := map[string]string{
		"TMPDIR":             temporaryRoot,
		DarwinRuntimeIDEnv:   generation.RuntimeID,
		DarwinScratchRootEnv: generation.ScratchRoot,
	}
	filtered := make([]string, 0, len(command.Env)+len(overrides))
	seen := make(map[string]bool, len(overrides))

	for _, entry := range command.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
			continue
		}

		if value, replace := overrides[key]; replace {
			entry = key + "=" + value
			seen[key] = true
		}

		filtered = append(filtered, entry)
	}

	for key, value := range overrides {
		if !seen[key] {
			filtered = append(filtered, key+"="+value)
		}
	}

	command.Env = filtered

	return nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)

	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func withoutPrivateAdapterEnv(entries []string) []string {
	environment := make([]string, 0, len(entries))
	for _, entry := range entries {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
			continue
		}

		environment = append(environment, entry)
	}

	return environment
}

func (generation *DarwinGeneration) started(pid, pgid int) error {
	if generation == nil || generation.RecordStarted == nil {
		return nil
	}

	return generation.RecordStarted(pid, pgid)
}

// Finish completes and releases the generation. It is memoized, so a caller
// that unwinds before handing the generation to a native launch may call it
// without racing the launch path's own completion.
func (generation *DarwinGeneration) Finish(complete bool) error {
	return generation.finish(complete)
}

func (generation *DarwinGeneration) finish(complete bool) error {
	if generation == nil {
		return nil
	}

	generation.finishOnce.Do(func() {
		if generation.RecordFinished != nil {
			if err := generation.RecordFinished(complete); err != nil {
				generation.finishErr = fmt.Errorf("%w: update Darwin containment record: %v", ErrProcessContainmentIncomplete, err)

				return
			}
		}

		if generation.Release != nil {
			generation.finishErr = errors.Join(generation.finishErr, generation.Release(complete))
		}
	})

	return generation.finishErr
}
