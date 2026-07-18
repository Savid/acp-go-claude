//go:build !darwin

package claude

import (
	"context"
	"os/exec"
)

func newProcessCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, path, args...) // #nosec G204 -- launches the configured Claude binary.
}
