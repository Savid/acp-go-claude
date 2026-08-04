//go:build !unix

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

func prepareProcessTreeCommand(cmd *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
	if err := validateProcessIsolation(options.Isolation); err != nil {
		return nil, err
	}
	if options.DarwinBestEffort {
		return nil, fmt.Errorf("%w: Darwin best-effort containment is invalid on %s", ErrProcessContainmentIncomplete, runtime.GOOS)
	}

	return &processTreeCommand{cmd: cmd}, nil
}

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	if launch != nil {
		launch.close()
	}

	return nil, fmt.Errorf("claude runtime containment is unsupported on %s", runtime.GOOS)
}

func (*processContainment) quiesce(time.Duration) error {
	return errors.New("claude runtime containment is unavailable")
}

func (*processContainment) close() error { return nil }

func (*processContainment) processSnapshot() (int, bool) { return 0, false }

func (*processContainment) wait(command *exec.Cmd) error { return command.Wait() }

func (*processContainment) ownsShutdown() bool { return false }

// configureProcessCommandPlatform has nothing to arm on a platform that cannot
// prove containment at all: startContainedProcess refuses to launch here.
func configureProcessCommandPlatform(*exec.Cmd) {}

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
