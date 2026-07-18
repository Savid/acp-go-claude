//go:build unix

package claude

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

var (
	signalOSProcess = func(process *os.Process, signal os.Signal) error {
		return process.Signal(signal)
	}
	syscallGetpgid = syscall.Getpgid
	syscallKill    = syscall.Kill
)

func configureProcessCommandPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = processSysProcAttr()
	configureProcessCommandCancel(cmd)
}

func terminateProcess(cmd *exec.Cmd) (bool, error) {
	return signalProcess(cmd, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) (bool, error) {
	return signalProcess(cmd, syscall.SIGKILL)
}

func signalProcess(cmd *exec.Cmd, processSignal syscall.Signal) (bool, error) {
	if cmd == nil || cmd.Process == nil {
		return false, nil
	}

	if usesProcessGroup(cmd) {
		return signalProcessGroup(cmd, processSignal)
	}

	if err := signalOSProcess(cmd.Process, processSignal); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func signalProcessGroup(cmd *exec.Cmd, processSignal syscall.Signal) (bool, error) {
	pgid, err := syscallGetpgid(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}

		return false, err
	}

	if err := syscallKill(-pgid, processSignal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func signalProcessGroupID(pgid int, processSignal syscall.Signal) error {
	if pgid <= 0 {
		return nil
	}

	if err := syscallKill(-pgid, processSignal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

func usesProcessGroup(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid
}
