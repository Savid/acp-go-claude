//go:build darwin

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type processContainment struct {
	ordinary       *ordinaryBoundary
	process        *os.Process
	processGroupID int
	waiter         *commandWait
	generation     *DarwinGeneration
	cleanupOnce    sync.Once
	cleanupErr     error
}

var (
	darwinContainmentNow     = time.Now
	darwinContainmentSleep   = time.Sleep
	darwinFastExitWait       = defaultCloseWait
	darwinAbortWait          = defaultCloseWait
	darwinAbortKillAfter     = defaultCloseKillAfter
	darwinDirectSignal       = func(process *os.Process, signal os.Signal) error { return process.Signal(signal) }
	darwinDirectKill         = func(process *os.Process) error { return process.Kill() }
	startPausedCommandWaitFn = startPausedCommandWait
)

func startContainedProcess(launch *processTreeCommand) (*processContainment, error) {
	if launch != nil && launch.ordinary {
		boundary, err := startOrdinaryBoundary(launch)
		if err != nil {
			return nil, err
		}

		return &processContainment{ordinary: boundary}, nil
	}

	if launch == nil || launch.cmd == nil || !launch.bestEffort || launch.generation == nil {
		if launch != nil {
			launch.close()
		}

		return nil, fmt.Errorf("%w: Darwin containment launch is unavailable", ErrProcessContainmentIncomplete)
	}

	if launch.cmd.SysProcAttr == nil || !launch.cmd.SysProcAttr.Setpgid {
		launch.close()

		return nil, fmt.Errorf("%w: Darwin native process group is not configured", ErrProcessContainmentIncomplete)
	}

	if err := launch.cmd.Start(); err != nil {
		finishErr := launch.generation.finish(true)
		launch.close()

		return nil, errors.Join(err, finishErr)
	}

	launch.releaseInherited()

	waiter, beginWait := startPausedCommandWaitFn(launch.cmd.Wait)
	tree := &processContainment{
		process:        launch.cmd.Process,
		processGroupID: launch.cmd.Process.Pid,
		waiter:         waiter,
		generation:     launch.generation,
	}

	pgid, pgidErr := syscallGetpgid(launch.cmd.Process.Pid)
	if errors.Is(pgidErr, syscall.ESRCH) {
		return nil, errors.Join(
			errors.New("claude launch exited before Darwin process-group identity validation"),
			handleDarwinFastExit(launch, tree, beginWait),
		)
	}

	if pgidErr != nil || pgid != launch.cmd.Process.Pid {
		launch.abortStartGate()
		_ = darwinDirectSignal(launch.cmd.Process, syscall.SIGTERM)

		beginWait()

		return nil, errors.Join(
			fmt.Errorf("%w: validate Darwin process-group leader pid=%d pgid=%d: %v", ErrProcessContainmentIncomplete, launch.cmd.Process.Pid, pgid, pgidErr),
			tree.abortUnvalidated(),
		)
	}

	tree.processGroupID = pgid

	if recordErr := tree.generation.started(launch.cmd.Process.Pid, pgid); recordErr != nil {
		launch.abortStartGate()

		return nil, errors.Join(
			fmt.Errorf("%w: record validated Darwin native launch: %v", ErrProcessContainmentIncomplete, recordErr),
			tree.cleanupFromProtectedObservation(beginWait),
		)
	}

	if gateErr := launch.releaseStartGate(); gateErr != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: release validated Darwin native launch: %v", ErrProcessContainmentIncomplete, gateErr),
			tree.cleanupFromProtectedObservation(beginWait),
		)
	}

	beginWait()

	if err := awaitProcessTreeReady(launch); err != nil {
		cleanupErr := tree.complete(defaultCloseWait)

		launch.close()

		return nil, errors.Join(err, cleanupErr)
	}

	return tree, nil
}

func handleDarwinFastExit(launch *processTreeCommand, tree *processContainment, beginWait func()) error {
	launch.abortStartGate()

	probeErr := syscallKill(-tree.processGroupID, 0)
	switch {
	case errors.Is(probeErr, syscall.ESRCH):
		beginWait()

		ctx, cancel := context.WithTimeout(context.Background(), darwinFastExitWait)
		defer cancel()

		waitErr, completed := tree.waiter.await(ctx)
		if !completed {
			return tree.finish(fmt.Errorf("%w: reap fast-exit Darwin child: %v", ErrProcessContainmentIncomplete, waitErr))
		}

		return errors.Join(waitErr, tree.finish(nil))
	case probeErr == nil || errors.Is(probeErr, syscall.EPERM):
		return tree.cleanupFromProtectedObservation(beginWait)
	default:
		_ = darwinDirectSignal(tree.process, syscall.SIGTERM)

		beginWait()

		return errors.Join(
			fmt.Errorf("%w: probe expected Darwin process group %d: %v", ErrProcessContainmentIncomplete, tree.processGroupID, probeErr),
			tree.abortUnvalidated(),
		)
	}
}

func (tree *processContainment) cleanupFromProtectedObservation(beginWait func()) error {
	tree.cleanupOnce.Do(func() {
		deadline := darwinContainmentNow().Add(defaultCloseWait)
		termDeadline := darwinContainmentNow().Add(defaultCloseKillAfter)
		termErr := syscallKill(-tree.processGroupID, syscall.SIGTERM)

		beginWait()

		tree.cleanupErr = tree.runCleanupAfterTerm(deadline, termDeadline, termErr)
	})

	return tree.cleanupErr
}

// complete ends the selected boundary. The ordinary boundary completes when the
// directly owned child does; the best-effort boundary drives the whole original
// process group to a stop first, which is as much as Darwin lets it prove.
func (tree *processContainment) complete(timeout time.Duration) error {
	if tree != nil && tree.ordinary != nil {
		return tree.ordinary.complete(timeout)
	}

	if tree == nil || tree.processGroupID <= 0 {
		return fmt.Errorf("%w: Darwin process-group identity is unavailable", ErrProcessContainmentIncomplete)
	}

	tree.cleanupOnce.Do(func() {
		deadline := darwinContainmentNow().Add(defaultCloseWait)
		termDeadline := darwinContainmentNow().Add(defaultCloseKillAfter)
		termErr := syscallKill(-tree.processGroupID, syscall.SIGTERM)
		tree.cleanupErr = tree.runCleanupAfterTerm(deadline, termDeadline, termErr)
	})

	return tree.cleanupErr
}

func (tree *processContainment) runCleanupAfterTerm(deadline, termDeadline time.Time, termErr error) error {
	groupAbsent := errors.Is(termErr, syscall.ESRCH)
	if termErr != nil && !groupAbsent && !errors.Is(termErr, syscall.EPERM) {
		return tree.failCleanup(deadline, fmt.Errorf("terminate original process group %d: %w", tree.processGroupID, termErr))
	}

	killed := false

	for darwinContainmentNow().Before(deadline) {
		if !groupAbsent {
			probeErr := syscallKill(-tree.processGroupID, 0)
			switch {
			case errors.Is(probeErr, syscall.ESRCH):
				groupAbsent = true
			case probeErr != nil && !errors.Is(probeErr, syscall.EPERM):
				return tree.failCleanup(deadline, fmt.Errorf("inspect original process group %d: %w", tree.processGroupID, probeErr))
			}
		}

		reaped := false

		if tree.waiter != nil {
			select {
			case <-tree.waiter.done:
				reaped = true
			default:
			}
		}

		if groupAbsent && reaped {
			return tree.finish(nil)
		}

		if !groupAbsent && !killed && !darwinContainmentNow().Before(termDeadline) {
			killErr := syscallKill(-tree.processGroupID, syscall.SIGKILL)
			if errors.Is(killErr, syscall.ESRCH) {
				groupAbsent = true
			} else if killErr != nil && !errors.Is(killErr, syscall.EPERM) {
				return tree.failCleanup(deadline, fmt.Errorf("kill original process group %d: %w", tree.processGroupID, killErr))
			}

			killed = true
		}

		darwinContainmentSleep(10 * time.Millisecond)
	}

	return tree.failCleanup(deadline, fmt.Errorf("direct child or original process group %d remained observable", tree.processGroupID))
}

func (tree *processContainment) abortUnvalidated() error {
	deadline := darwinContainmentNow().Add(darwinAbortWait)
	termDeadline := darwinContainmentNow().Add(darwinAbortKillAfter)
	termErr := darwinDirectSignal(tree.process, syscall.SIGTERM)

	ctx, cancel := context.WithDeadline(context.Background(), termDeadline)
	_, completed := tree.waiter.await(ctx)

	cancel()

	if !completed {
		killErr := darwinDirectKill(tree.process)
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			termErr = errors.Join(termErr, fmt.Errorf("kill unvalidated direct child: %w", killErr))
		}

		ctx, cancel = context.WithDeadline(context.Background(), deadline)
		_, completed = tree.waiter.await(ctx)

		cancel()
	}

	var reapErr error
	if !completed {
		reapErr = errors.New("direct Claude child was not reaped before the fixed cleanup deadline")
	}

	return tree.finish(errors.Join(
		ErrProcessContainmentIncomplete,
		termErr,
		reapErr,
	))
}

func (tree *processContainment) failCleanup(deadline time.Time, cause error) error {
	var killErr error
	if tree.process != nil {
		killErr = darwinDirectKill(tree.process)
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
	}

	var reapErr error

	if tree.waiter != nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, completed := tree.waiter.await(ctx)

		cancel()

		if !completed {
			reapErr = errors.New("direct Claude child was not reaped before the fixed cleanup deadline")
		}
	}

	return tree.finish(errors.Join(
		fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, cause),
		killErr,
		reapErr,
	))
}

func (tree *processContainment) finish(err error) error {
	complete := !errors.Is(err, ErrProcessContainmentIncomplete)

	return errors.Join(err, tree.generation.finish(complete))
}

func (tree *processContainment) wait(*exec.Cmd) error {
	if tree != nil && tree.ordinary != nil {
		return tree.ordinary.wait()
	}

	containmentErr := tree.complete(defaultCloseWait)

	var waitErr error

	select {
	case <-tree.waiter.done:
		waitErr = tree.waiter.err
	default:
		waitErr = fmt.Errorf("%w: direct Claude child was not reaped", ErrProcessContainmentIncomplete)
	}

	if containmentErr == nil && errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}

	return errors.Join(waitErr, containmentErr)
}

// ownsShutdown reports whether the boundary drives shutdown itself. The
// best-effort group boundary does; the ordinary boundary leaves the direct
// child to the caller's own termination ladder.
func (tree *processContainment) ownsShutdown() bool {
	return tree == nil || tree.ordinary == nil
}

func (*processContainment) close() error { return nil }

func (*processContainment) processSnapshot() (int, bool) { return 0, false }
