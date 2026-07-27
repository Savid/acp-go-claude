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
}
