//go:build unix && !linux

package claude

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type processContainment struct{}

func prepareProcessTreeCommand(*exec.Cmd) (*processTreeCommand, error) {
	return nil, fmt.Errorf(
		"%w: %s cannot prove Claude descendants that escape a process group",
		ErrProcessTreeUnproven,
		runtime.GOOS,
	)
}

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	if launch != nil {
		launch.close()
	}

	return nil, ErrProcessTreeUnproven
}

func (*processContainment) quiesce(time.Duration) error { return ErrProcessTreeUnproven }
func (*processContainment) close() error                { return nil }
func (*processContainment) processSnapshot() (int, bool) {
	return 0, false
}
