//go:build unix && !darwin

package claude

import (
	"os/exec"
	"syscall"
)

func configureProcessCommandCancel(command *exec.Cmd) {
	command.Cancel = func() error {
		_, err := signalProcess(command, syscall.SIGTERM)

		return err
	}
}
