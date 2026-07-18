//go:build darwin

package claude

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestDarwinUnixSignalProcessBranches(t *testing.T) {
	if signaled, err := signalProcess(nil, syscall.SIGTERM); err != nil || signaled {
		t.Fatalf("nil signal = (%v,%v)", signaled, err)
	}
	if signaled, err := signalProcess(&exec.Cmd{}, syscall.SIGTERM); err != nil || signaled {
		t.Fatalf("empty signal = (%v,%v)", signaled, err)
	}

	originalSignal := signalOSProcess
	t.Cleanup(func() { signalOSProcess = originalSignal })
	command := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	signalOSProcess = func(*os.Process, os.Signal) error { return os.ErrProcessDone }
	if signaled, err := signalProcess(command, syscall.SIGTERM); err != nil || signaled {
		t.Fatalf("done signal = (%v,%v)", signaled, err)
	}
	want := errors.New("signal")
	signalOSProcess = func(*os.Process, os.Signal) error { return want }
	if signaled, err := signalProcess(command, syscall.SIGTERM); !errors.Is(err, want) || signaled {
		t.Fatalf("failed signal = (%v,%v)", signaled, err)
	}
	signalOSProcess = func(*os.Process, os.Signal) error { return nil }
	if signaled, err := signalProcess(command, syscall.SIGTERM); err != nil || !signaled {
		t.Fatalf("successful signal = (%v,%v)", signaled, err)
	}
}

func TestDarwinUnixSignalProcessGroupBranches(t *testing.T) {
	originalGetpgid := syscallGetpgid
	originalKill := syscallKill
	t.Cleanup(func() {
		syscallGetpgid = originalGetpgid
		syscallKill = originalKill
	})
	command := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}

	syscallGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	if signaled, err := signalProcess(command, syscall.SIGTERM); err != nil || signaled {
		t.Fatalf("gone group = (%v,%v)", signaled, err)
	}
	want := errors.New("getpgid")
	syscallGetpgid = func(int) (int, error) { return 0, want }
	if signaled, err := signalProcess(command, syscall.SIGTERM); !errors.Is(err, want) || signaled {
		t.Fatalf("getpgid failure = (%v,%v)", signaled, err)
	}
	syscallGetpgid = func(int) (int, error) { return 42, nil }
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if signaled, err := signalProcessGroup(command, syscall.SIGTERM); err != nil || signaled {
		t.Fatalf("gone signal group = (%v,%v)", signaled, err)
	}
	syscallKill = func(int, syscall.Signal) error { return want }
	if signaled, err := signalProcessGroup(command, syscall.SIGTERM); !errors.Is(err, want) || signaled {
		t.Fatalf("signal group failure = (%v,%v)", signaled, err)
	}
	syscallKill = func(pid int, signal syscall.Signal) error {
		if pid != -42 || signal != syscall.SIGTERM {
			t.Fatalf("signal target=(%d,%v)", pid, signal)
		}

		return nil
	}
	if signaled, err := signalProcessGroup(command, syscall.SIGTERM); err != nil || !signaled {
		t.Fatalf("successful group signal = (%v,%v)", signaled, err)
	}
}

func TestDarwinUnixSignalProcessGroupIDBranches(t *testing.T) {
	originalKill := syscallKill
	t.Cleanup(func() { syscallKill = originalKill })
	if err := signalProcessGroupID(0, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := signalProcessGroupID(42, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	want := errors.New("signal")
	syscallKill = func(int, syscall.Signal) error { return want }
	if err := signalProcessGroupID(42, syscall.SIGTERM); !errors.Is(err, want) {
		t.Fatalf("group id error = %v", err)
	}
	syscallKill = func(int, syscall.Signal) error { return nil }
	if err := signalProcessGroupID(42, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinConfigureCancelIsNoop(t *testing.T) {
	command := exec.Command("true")
	configureProcessCommandCancel(command)
	if command.Cancel != nil {
		t.Fatal("Darwin command gained a parallel cancellation path")
	}
}
