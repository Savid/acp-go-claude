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

// startChildExit starts the observation of the direct child's own exit. Every
// boundary outside Darwin leaves the reap to whoever owns the child, so this is
// that reap, and the owner collects its result instead of waiting a second
// time. The ordinary boundary holds that sole reap paused until someone owns
// the wait, so this starts it and hands the same observation back. It signals
// only that the child ended; it terminates nothing, so the containment boundary
// stays the sole authoritative shutdown channel.
func startChildExit(tree *processContainment, cmd *exec.Cmd) *commandWait {
	if waiter := tree.ordinary.observeExit(); waiter != nil {
		return waiter
	}

	wait, begin := startPausedCommandWait(cmd.Wait)
	begin()

	return wait
}
