//go:build !darwin

package claude

import (
	"os"
	"os/exec"
)

// processTreeCommand owns the platform launch wrapper and the parent-side
// descriptors that establish the containment boundary.
type processTreeCommand struct {
	cmd       *exec.Cmd
	inherited []*os.File
	startGate *os.File
	control   *os.File
	ready     *os.File
	proof     *os.File
	ordinary  bool
}

// prepareChildExit returns the boundary's sole direct-child waiter and its
// idempotent arm. Ordinary execution leaves the wait paused for the owner of
// the command's pipes. The hardened Linux helper has no competing parent pipe,
// so its existing eager observation remains armed.
func prepareChildExit(tree *processContainment, cmd *exec.Cmd) (*commandWait, func()) {
	if tree != nil && tree.ordinary != nil {
		return tree.ordinary.waiter, tree.ordinary.beginWait
	}

	waiter, begin := startPausedCommandWait(cmd.Wait)
	begin()

	return waiter, begin
}

func startChildExit(tree *processContainment, command *exec.Cmd) *commandWait {
	waiter, begin := prepareChildExit(tree, command)
	begin()

	return waiter
}
