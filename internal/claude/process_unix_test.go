//go:build unix

package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigureProcessCommandSetsUnixProcessGroupAndCancel(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")

	configureProcessCommand(cmd)

	require.Equal(t, processShutdownWaitDelay, cmd.WaitDelay)
	require.NotNil(t, cmd.Cancel)
	require.True(t, usesProcessGroup(cmd))
	require.NoError(t, cmd.Cancel())
}

func TestUnixSignalProcessBranches(t *testing.T) {
	signaled, err := signalProcess(nil, syscall.SIGTERM)
	require.NoError(t, err)
	require.False(t, signaled)

	signaled, err = signalProcess(&exec.Cmd{}, syscall.SIGTERM)
	require.NoError(t, err)
	require.False(t, signaled)

	oldSignalOSProcess := signalOSProcess
	signalOSProcess = func(*os.Process, os.Signal) error {
		return errors.New("signal failed")
	}
	t.Cleanup(func() { signalOSProcess = oldSignalOSProcess })

	signaled, err = signalProcess(&exec.Cmd{Process: &os.Process{}}, syscall.SIGTERM)
	require.Error(t, err)
	require.False(t, signaled)
}

func TestUnixSignalProcessGroupErrors(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}

	oldGetpgid := syscallGetpgid
	oldKill := syscallKill
	t.Cleanup(func() {
		syscallGetpgid = oldGetpgid
		syscallKill = oldKill
	})

	syscallGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	signaled, err := signalProcessGroup(cmd, syscall.SIGTERM)
	require.NoError(t, err)
	require.False(t, signaled)

	syscallGetpgid = func(int) (int, error) { return 0, errors.New("getpgid failed") }
	signaled, err = signalProcessGroup(cmd, syscall.SIGTERM)
	require.Error(t, err)
	require.False(t, signaled)

	syscallGetpgid = func(int) (int, error) { return os.Getpid(), nil }
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	signaled, err = signalProcessGroup(cmd, syscall.SIGTERM)
	require.NoError(t, err)
	require.False(t, signaled)

	syscallKill = func(int, syscall.Signal) error { return errors.New("kill failed") }
	signaled, err = signalProcessGroup(cmd, syscall.SIGTERM)
	require.Error(t, err)
	require.False(t, signaled)
}

func TestUnixProcessContainmentProofBranches(t *testing.T) {
	if _, err := startContainedProcess(exec.Command("sh", "-c", "exit 0")); err == nil {
		t.Fatal("startContainedProcess accepted a command without a process group")
	}

	missing := exec.Command(filepath.Join(t.TempDir(), "missing"))
	configureProcessCommand(missing)
	if _, err := startContainedProcess(missing); err == nil {
		t.Fatal("startContainedProcess accepted a missing executable")
	}

	require.Error(t, (*processContainment)(nil).quiesce(time.Millisecond))
	require.Error(t, (&processContainment{}).quiesce(time.Millisecond))

	oldKill := syscallKill
	t.Cleanup(func() { syscallKill = oldKill })
	tree := &processContainment{processGroupID: 123}

	for _, tc := range []struct {
		name  string
		err   error
		alive bool
		bad   bool
	}{
		{name: "alive", alive: true},
		{name: "permission", err: syscall.EPERM, alive: true},
		{name: "gone", err: syscall.ESRCH},
		{name: "failure", err: errors.New("probe failed"), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syscallKill = func(int, syscall.Signal) error { return tc.err }
			alive, err := tree.alive()
			require.Equal(t, tc.alive, alive)
			require.Equal(t, tc.bad, err != nil)
		})
	}

	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, tree.signal(syscall.SIGTERM))
	require.NoError(t, tree.quiesce(0))

	syscallKill = func(int, syscall.Signal) error { return errors.New("signal failed") }
	require.Error(t, tree.signal(syscall.SIGTERM))
	require.Error(t, tree.waitUntilEmpty(time.Now().Add(time.Second)))

	syscallKill = func(int, syscall.Signal) error { return nil }
	require.Error(t, tree.waitUntilEmpty(time.Now().Add(-time.Second)))
	require.Error(t, tree.quiesce(time.Nanosecond))

	probeCalls := 0
	syscallKill = func(_ int, signal syscall.Signal) error {
		if signal != 0 {
			return nil
		}

		probeCalls++
		if probeCalls == 1 {
			return errors.New("first probe failed")
		}

		return syscall.ESRCH
	}
	require.NoError(t, tree.quiesce(time.Second))
}

func TestProcessTransportQuiescenceProofFailures(t *testing.T) {
	oldKill := syscallKill
	oldClose := processContainmentClose
	t.Cleanup(func() {
		syscallKill = oldKill
		processContainmentClose = oldClose
	})

	transport := &ProcessTransport{tree: &processContainment{processGroupID: 123}}
	syscallKill = func(int, syscall.Signal) error { return errors.New("probe failed") }
	require.ErrorIs(t, transport.quiesceProcessTree(), ErrProcessTreeUnproven)

	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	processContainmentClose = func(*processContainment) error { return errors.New("close failed") }
	require.ErrorContains(t, transport.quiesceProcessTree(), "close Claude process containment")
}

func TestProcessTransportCloseKillsUnixProcessGroup(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 20 * time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	oldGrace := processExitGracePeriod
	processExitGracePeriod = 20 * time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap '' TERM
sh -c 'trap "" TERM; while :; do sleep 1; done' &
echo $! > "$CHILD_PID_FILE"
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, Options{
		CLIPath: script,
		Env:     map[string]string{"CHILD_PID_FILE": pidFile},
	})

	require.NoError(t, transport.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(pidFile)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)

	require.NoError(t, transport.Close())
	require.Eventually(t, func() bool {
		return !processExists(childPID)
	}, time.Second, 10*time.Millisecond)
}

func TestProcessTransportCloseProvesQuiescenceAfterRootExit(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 2 * time.Second
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
sh -c 'trap "" TERM; while :; do sleep 1; done' &
echo $! > "$CHILD_PID_FILE"
exit 0
`)
	transport := NewProcessTransport(nil, Options{
		CLIPath: script,
		Env:     map[string]string{"CHILD_PID_FILE": pidFile},
	})

	require.NoError(t, transport.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(pidFile)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)

	require.NoError(t, transport.Close())
	require.False(t, processExists(childPID), "Close returned before the post-root descendant exited")
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || err == syscall.EPERM
}
