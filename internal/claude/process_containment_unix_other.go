//go:build unix && !linux && !darwin

package claude

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type processContainment struct {
	ordinary *ordinaryBoundary
}

func prepareProcessTreeCommand(native *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
	if options.Isolation != nil {
		return nil, fmt.Errorf(
			"%w: explicit process isolation is unsupported on %s",
			ErrProcessContainmentIncomplete,
			runtime.GOOS,
		)
	}

	if options.DarwinBestEffort {
		return nil, fmt.Errorf(
			"%w: Darwin best-effort containment is invalid on %s",
			ErrProcessContainmentIncomplete,
			runtime.GOOS,
		)
	}

	return prepareOrdinaryLaunch(native, options)
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
func (*processContainment) processSnapshot() (int, bool) {
	return 0, false
}
func (c *processContainment) wait(*exec.Cmd) error { return c.ordinary.wait() }
func (*processContainment) ownsShutdown() bool     { return false }
