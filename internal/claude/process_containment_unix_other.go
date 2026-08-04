//go:build unix && !linux && !darwin

package claude

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type processContainment struct{}

func prepareProcessTreeCommand(_ *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
	if err := validateProcessIsolation(options.Isolation); err != nil {
		return nil, err
	}
	if options.DarwinBestEffort {
		return nil, fmt.Errorf(
			"%w: Darwin best-effort containment is invalid on %s",
			ErrProcessContainmentIncomplete,
			runtime.GOOS,
		)
	}

	return nil, fmt.Errorf(
		"%w: %s cannot prove Claude descendants that escape a process group",
		ErrProcessContainmentIncomplete,
		runtime.GOOS,
	)
}

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	if launch != nil {
		launch.close()
	}

	return nil, ErrProcessContainmentIncomplete
}

func (*processContainment) quiesce(time.Duration) error { return ErrProcessContainmentIncomplete }
func (*processContainment) close() error                { return nil }
func (*processContainment) processSnapshot() (int, bool) {
	return 0, false
}
func (*processContainment) wait(command *exec.Cmd) error { return command.Wait() }
func (*processContainment) ownsShutdown() bool           { return false }
