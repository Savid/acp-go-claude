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
	control   *os.File
	ready     *os.File
	proof     *os.File
}

func (c *processTreeCommand) releaseInherited() {
	if c == nil {
		return
	}

	for _, file := range c.inherited {
		_ = file.Close()
	}

	c.inherited = nil
}

func (c *processTreeCommand) close() {
	if c == nil {
		return
	}

	c.releaseInherited()

	if c.control != nil {
		_ = c.control.Close()
		c.control = nil
	}

	if c.ready != nil {
		_ = c.ready.Close()
		c.ready = nil
	}

	if c.proof != nil {
		_ = c.proof.Close()
		c.proof = nil
	}
}
