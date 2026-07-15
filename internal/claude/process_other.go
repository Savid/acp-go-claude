//go:build !unix && !windows

package claude

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

type processContainment struct{}

func startContainedProcess(*exec.Cmd) (*processContainment, error) {
	return nil, fmt.Errorf("claude runtime containment is unsupported on %s", runtime.GOOS)
}

func (*processContainment) quiesce(time.Duration) error {
	return errors.New("claude runtime containment is unavailable")
}

func (*processContainment) close() error { return nil }

func (*processContainment) processSnapshot() (int, bool) { return 0, false }

func configureProcessCommandPlatform(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		_, err := killProcess(cmd)

		return err
	}
}

func terminateProcess(cmd *exec.Cmd) (bool, error) {
	return killProcess(cmd)
}

func killProcess(cmd *exec.Cmd) (bool, error) {
	if cmd == nil || cmd.Process == nil {
		return false, nil
	}

	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}
