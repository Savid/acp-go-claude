package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

var authDirectContainmentOwners sync.Map

// useAuthDirectContainment replaces the Darwin containment boundary with a
// direct process-group launch for the auth legs. The real boundary re-executes
// a bootstrap helper under a credential whose supplementary groups are cleared,
// which no unprivileged Darwin process can do, so the auth flows would otherwise
// go unexercised on the platform that owns them.
func useAuthDirectContainment(t *testing.T) {
	t.Helper()

	if _, loaded := authDirectContainmentOwners.LoadOrStore(t, struct{}{}); loaded {
		return
	}

	prepare := processPrepareContained
	start := processStartContained
	wait := processWaitContained
	quiesce := processBoundaryComplete
	closeContainment := processContainmentClose

	processPrepareContained = func(command *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
		if err := validateProcessIsolation(options.Isolation); err != nil {
			return nil, err
		}

		return &processTreeCommand{cmd: command, bestEffort: true, generation: options.Generation}, nil
	}
	processStartContained = func(launch *processTreeCommand) (*processContainment, error) {
		if err := launch.cmd.Start(); err != nil {
			return nil, err
		}

		waiter, begin := startPausedCommandWait(launch.cmd.Wait)
		begin()

		return &processContainment{
			process:        launch.cmd.Process,
			processGroupID: launch.cmd.Process.Pid,
			waiter:         waiter,
			generation:     launch.generation,
		}, nil
	}
	processWaitContained = func(tree *processContainment, _ *exec.Cmd) error {
		waitErr, _ := tree.waiter.await(context.Background())

		return waitErr
	}
	processBoundaryComplete = func(tree *processContainment, timeout time.Duration) error {
		// A group whose last member has already been reaped answers ESRCH, and
		// Darwin answers EPERM for one whose pid the kernel has not recycled yet;
		// the real boundary tolerates both for the same reason.
		err := syscall.Kill(-tree.processGroupID, syscall.SIGKILL)
		if err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if _, completed := tree.waiter.await(ctx); !completed {
			return fmt.Errorf("direct child %d was not reaped", tree.processGroupID)
		}

		return nil
	}
	processContainmentClose = func(*processContainment) error { return nil }

	t.Cleanup(func() {
		authDirectContainmentOwners.Delete(t)

		processPrepareContained = prepare
		processStartContained = start
		processWaitContained = wait
		processBoundaryComplete = quiesce
		processContainmentClose = closeContainment
	})
}
