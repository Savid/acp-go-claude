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
	bestEffort bool
	generation *DarwinGeneration
}

// startChildExit hands back the observation of the direct child's own exit. The
// Darwin boundary reaps the child from the moment it starts it, so this is that
// reap rather than a second one: two goroutines waiting on one command make the
// loser answer at once and wrongly, whatever the child is still doing.
func startChildExit(tree *processContainment, _ *exec.Cmd) *commandWait {
	return tree.waiter
}
