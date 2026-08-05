//go:build linux

package claude

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

var directTestContainmentOwners sync.Map

func useDirectTestContainment(t *testing.T) {
	t.Helper()
	if _, loaded := directTestContainmentOwners.LoadOrStore(t, struct{}{}); loaded {
		return
	}
	prepare := processPrepareContained
	start := processStartContained
	wait := processWaitContained
	quiesce := processContainmentQuiesce
	closeContainment := processContainmentClose
	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		return &processTreeCommand{cmd: command}, nil
	}
	processStartContained = func(launch *processTreeCommand) (*processContainment, error) {
		if err := launch.cmd.Start(); err != nil {
			return nil, err
		}

		return &processContainment{processGroupID: launch.cmd.Process.Pid}, nil
	}
	processWaitContained = func(_ *processContainment, command *exec.Cmd) error { return command.Wait() }
	processContainmentQuiesce = func(tree *processContainment, _ time.Duration) error {
		if tree == nil || tree.processGroupID <= 0 {
			return nil
		}
		if err := syscall.Kill(-tree.processGroupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}

		return nil
	}
	processContainmentClose = func(*processContainment) error { return nil }
	t.Cleanup(func() {
		directTestContainmentOwners.Delete(t)
		processPrepareContained = prepare
		processStartContained = start
		processWaitContained = wait
		processContainmentQuiesce = quiesce
		processContainmentClose = closeContainment
	})
}
