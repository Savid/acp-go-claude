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

type processContainment struct {
	ordinary *ordinaryBoundary
}

func prepareProcessTreeCommand(cmd *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
	if options.Isolation != nil {
		return nil, fmt.Errorf(
			"%w: explicit process isolation is unsupported on %s",
			ErrProcessContainmentIncomplete,
			runtime.GOOS,
		)
	}

	if options.DarwinBestEffort {
		return nil, fmt.Errorf("%w: Darwin best-effort containment is invalid on %s", ErrProcessContainmentIncomplete, runtime.GOOS)
	}

	return prepareOrdinaryLaunch(cmd, options)
}

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	boundary, err := startOrdinaryBoundary(launch)
	if err != nil {
		return nil, err
	}

	return &processContainment{ordinary: boundary}, nil
}

func (c *processContainment) quiesce(timeout time.Duration) error {
	return c.ordinary.complete(timeout)
}

func (*processContainment) close() error { return nil }

func (*processContainment) processSnapshot() (int, bool) { return 0, false }

func (c *processContainment) wait(*exec.Cmd) error { return c.ordinary.wait() }

func (*processContainment) ownsShutdown() bool { return false }

// configureProcessCommandPlatform has no process group to arm here; the
// ordinary boundary signals the direct child itself.
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
