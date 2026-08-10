//go:build darwin

package claude

import (
	"os"
	"os/exec"
)

// processTreeCommand owns the platform launch wrapper and the parent-side
// descriptors that establish the containment boundary. The Darwin boundary also
// needs the operator's explicit acceptance and the generation record it reports
// its lifecycle to.
type processTreeCommand struct {
	cmd        *exec.Cmd
	inherited  []*os.File
	startGate  *os.File
	control    *os.File
	ready      *os.File
	proof      *os.File
	ordinary   bool
	bestEffort bool
	generation *DarwinGeneration
}

// startChildExit hands back the observation of the direct child's own exit.
// Every Darwin boundary owns exactly one reap of that child, so this is that
// reap rather than a second one: two goroutines waiting on one command make the
// loser answer at once and wrongly, whatever the child is still doing. The
// best-effort boundary already started its reap on the launch helper; the
// ordinary boundary holds its own paused until an owner appears, and an exit
// observer is one.
func startChildExit(tree *processContainment, _ *exec.Cmd) *commandWait {
	if waiter := tree.ordinary.observeExit(); waiter != nil {
		return waiter
	}

	return tree.waiter
}
