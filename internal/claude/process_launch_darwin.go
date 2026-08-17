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

// prepareChildExit returns the boundary's sole direct-child waiter and the
// operation that arms it. The ordinary waiter stays paused so a caller that
// owns a command pipe can finish reading first. Darwin best-effort already
// armed its waiter during process-group validation, so its arm is a no-op.
func prepareChildExit(tree *processContainment, _ *exec.Cmd) (*commandWait, func()) {
	if tree != nil && tree.ordinary != nil {
		return tree.ordinary.waiter, tree.ordinary.beginWait
	}

	return tree.waiter, func() {}
}

func startChildExit(tree *processContainment, command *exec.Cmd) *commandWait {
	waiter, begin := prepareChildExit(tree, command)
	begin()

	return waiter
}
