//go:build unix

package claude

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var (
	signalOSProcess = func(process *os.Process, signal os.Signal) error {
		return process.Signal(signal)
	}
	syscallGetpgid = syscall.Getpgid
	syscallKill    = syscall.Kill
)

type processContainment struct {
	processGroupID int
}

func startContainedProcess(cmd *exec.Cmd) (*processContainment, error) {
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return nil, errors.New("claude Unix process-group containment is not configured")
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &processContainment{processGroupID: cmd.Process.Pid}, nil
}

func (c *processContainment) quiesce(timeout time.Duration) error {
	if c == nil || c.processGroupID <= 0 {
		return errors.New("claude process-group identity is unavailable")
	}

	if timeout <= 0 {
		timeout = time.Second
	}

	deadline := time.Now().Add(timeout)
	_ = c.signal(syscall.SIGTERM)

	termDeadline := time.Now().Add(500 * time.Millisecond)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	if err := c.waitUntilEmpty(termDeadline); err == nil {
		return nil
	}

	_ = c.signal(syscall.SIGKILL)

	if err := c.waitUntilEmpty(deadline); err != nil {
		return fmt.Errorf("claude process group %d did not become quiescent: %w", c.processGroupID, err)
	}

	return nil
}

func (c *processContainment) waitUntilEmpty(deadline time.Time) error {
	for {
		alive, err := c.alive()
		if err != nil {
			return err
		}

		if !alive {
			return nil
		}

		if !time.Now().Before(deadline) {
			return errors.New("deadline exceeded")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (c *processContainment) alive() (bool, error) {
	err := syscallKill(-c.processGroupID, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("probe Claude process group %d: %w", c.processGroupID, err)
	}
}

func (c *processContainment) signal(signal syscall.Signal) error {
	err := syscallKill(-c.processGroupID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

func (*processContainment) close() error { return nil }

func (*processContainment) processSnapshot() (int, bool) { return 0, false }

func configureProcessCommandPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = processSysProcAttr()
	cmd.Cancel = func() error {
		_, err := signalProcess(cmd, syscall.SIGTERM)

		return err
	}
}

func terminateProcess(cmd *exec.Cmd) (bool, error) {
	return signalProcess(cmd, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) (bool, error) {
	return signalProcess(cmd, syscall.SIGKILL)
}

func signalProcess(cmd *exec.Cmd, signal syscall.Signal) (bool, error) {
	if cmd == nil || cmd.Process == nil {
		return false, nil
	}

	if usesProcessGroup(cmd) {
		return signalProcessGroup(cmd, signal)
	}

	if err := signalOSProcess(cmd.Process, signal); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) (bool, error) {
	pgid, err := syscallGetpgid(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}

		return false, err
	}

	if err := syscallKill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func usesProcessGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}
