//go:build darwin

package claude

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

var directDarwinTestContainment sync.Mutex
var directDarwinTestContainmentOwners sync.Map

func useDirectTestContainment(t *testing.T) {
	t.Helper()
	if _, loaded := directDarwinTestContainmentOwners.LoadOrStore(t, struct{}{}); loaded {
		return
	}
	directDarwinTestContainment.Lock()
	prepare := processPrepareContained
	start := processStartContained
	wait := processWaitContained
	quiesce := processContainmentQuiesce
	closeContainment := processContainmentClose
	processPrepareContained = func(command *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
		return &processTreeCommand{cmd: command, generation: options.Generation}, nil
	}
	processStartContained = func(launch *processTreeCommand) (*processContainment, error) {
		if err := launch.cmd.Start(); err != nil {
			return nil, err
		}
		waiter, beginWait := startPausedCommandWait(launch.cmd.Wait)
		beginWait()
		generation := launch.generation
		if generation == nil {
			generation = &DarwinGeneration{}
		}

		return &processContainment{
			process: launch.cmd.Process, processGroupID: launch.cmd.Process.Pid,
			waiter: waiter, generation: generation,
		}, nil
	}
	processWaitContained = func(tree *processContainment, command *exec.Cmd) error { return tree.wait(command) }
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
		directDarwinTestContainmentOwners.Delete(t)
		processPrepareContained = prepare
		processStartContained = start
		processWaitContained = wait
		processContainmentQuiesce = quiesce
		processContainmentClose = closeContainment
		directDarwinTestContainment.Unlock()
	})
}
