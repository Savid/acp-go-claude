//go:build darwin

package claude

import (
	"context"
	"os/exec"
)

func newProcessCommand(_ context.Context, path string, args ...string) *exec.Cmd {
	return exec.Command(path, args...) // #nosec G204 -- launches the configured Claude binary.
}

func configureProcessCommandCancel(command *exec.Cmd) { _ = command }
