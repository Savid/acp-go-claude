//go:build linux

package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const supervisorDetachedChildArg = "--claude-supervisor-detached-child"

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

func TestLinuxProcessContainmentProofBranches(t *testing.T) {
	require.Error(t, (*processContainment)(nil).quiesce(time.Millisecond))
	require.Error(t, (&processContainment{}).quiesce(time.Millisecond))
	require.NoError(t, (*processContainment)(nil).close())

	oldKill := syscallKill
	t.Cleanup(func() { syscallKill = oldKill })

	read, write, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	tree := &processContainment{processGroupID: 123, control: write, proof: supervisorProofFile(t)}
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, tree.quiesce(0))
	require.NoError(t, tree.quiesce(time.Second), "proof must be memoized")
	require.NoError(t, tree.close())
	count, exact := tree.processSnapshot()
	require.Zero(t, count)
	require.False(t, exact)

	read, write, err = os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	tree = &processContainment{processGroupID: 124, control: write, proof: supervisorProofFile(t)}
	syscallKill = func(int, syscall.Signal) error { return errors.New("probe failed") }
	require.Error(t, tree.quiesce(time.Second))

	tree = &processContainment{processGroupID: 125}
	require.Error(t, tree.quiesce(time.Second))

	read, write, err = os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	require.NoError(t, write.Close())
	tree = &processContainment{processGroupID: 125, control: write}
	require.Error(t, tree.quiesce(time.Second))

	// The deadline branch needs a group that still holds a running member, so
	// the test names its own: an unoccupied group now reports quiescence
	// whatever the signal probe is stubbed to return.
	group, err := syscall.Getpgid(os.Getpid())
	require.NoError(t, err)

	read, write, err = os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	tree = &processContainment{processGroupID: group, control: write, proof: supervisorProofFile(t)}
	syscallKill = func(int, syscall.Signal) error { return nil }
	require.Error(t, tree.quiesce(time.Nanosecond))
	require.Error(t, tree.waitUntilEmpty(time.Nanosecond))
}

// TestLinuxProcessContainmentTreatsAnUnreapedExitAsQuiesced pins the fact every
// contained shutdown depends on: the caller reaps the supervisor only after
// quiescence returns, so the supervisor is always an unreaped zombie at the
// moment it is probed, and kill(2) still addresses its group.
func TestLinuxProcessContainmentTreatsAnUnreapedExitAsQuiesced(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	require.NoError(t, command.Start())

	group := command.Process.Pid

	t.Cleanup(func() { _ = command.Wait() })

	tree := &processContainment{processGroupID: group}

	require.Eventually(t, func() bool {
		identity, err := readLinuxProcessIdentity(group)

		return err == nil && identity.state == 'Z'
	}, 5*time.Second, 10*time.Millisecond, "the child never became an unreaped exit")

	require.NoError(t, syscall.Kill(-group, 0), "an unreaped exit still answers a group signal probe")
	require.NoError(t, tree.waitUntilEmpty(time.Second))
}

func TestLinuxProcessContainmentCloseBranches(t *testing.T) {
	require.NoError(t, (*processContainment)(nil).close())

	proofRead, proofWrite, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, proofWrite.Close())
	tree := &processContainment{proof: proofRead}
	require.NoError(t, tree.close())

	controlRead, controlWrite, err := os.Pipe()
	require.NoError(t, err)
	proofRead, proofWrite, err = os.Pipe()
	require.NoError(t, err)
	tree = &processContainment{control: controlWrite, proof: proofRead}
	require.NoError(t, tree.close())
	require.NoError(t, controlRead.Close())
	require.NoError(t, proofWrite.Close())

	controlRead, controlWrite, err = os.Pipe()
	require.NoError(t, err)
	require.NoError(t, controlWrite.Close())
	tree = &processContainment{control: controlWrite}
	require.Error(t, tree.close())
	require.NoError(t, controlRead.Close())
}

func TestAwaitLinuxProcessSupervisorProofBranches(t *testing.T) {
	require.Error(t, awaitProcessTreeProof(nil, time.Second))

	regular, err := os.CreateTemp(t.TempDir(), "proof")
	require.NoError(t, err)
	require.Error(t, awaitProcessTreeProof(regular, time.Second))

	for _, test := range []struct {
		name  string
		value string
		ok    bool
	}{
		{name: "eof"},
		{name: "invalid", value: "wrong\n"},
		{name: "valid", value: turnSupervisorProof, ok: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			read, write, pipeErr := os.Pipe()
			require.NoError(t, pipeErr)
			if test.value != "" {
				_, pipeErr = write.WriteString(test.value)
				require.NoError(t, pipeErr)
			}
			require.NoError(t, write.Close())
			err := awaitProcessTreeProof(read, 0)
			require.Equal(t, test.ok, err == nil)
		})
	}
}

func TestStartLinuxProcessSupervisorBranches(t *testing.T) {
	(*processTreeCommand)(nil).releaseInherited()
	(*processTreeCommand)(nil).close()
	_, err := startContainedProcess(nil)
	require.Error(t, err)

	launch := &processTreeCommand{cmd: exec.Command("sh", "-c", "exit 0")}
	_, err = startContainedProcess(launch)
	require.Error(t, err)

	missing := exec.Command(filepath.Join(t.TempDir(), "missing"))
	missing.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_, err = startContainedProcess(&processTreeCommand{cmd: missing})
	require.Error(t, err)

	readyRead, readyWrite, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, readyWrite.Close())
	proofRead, proofWrite, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, proofWrite.Close())
	controlRead, controlWrite, err := os.Pipe()
	require.NoError(t, err)
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	launch = &processTreeCommand{
		cmd: cmd, inherited: []*os.File{controlRead},
		control: controlWrite, ready: readyRead, proof: proofRead,
	}
	_, err = startContainedProcess(launch)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}

func TestUnixSignalProcessGroupIDBranches(t *testing.T) {
	oldKill := syscallKill
	t.Cleanup(func() { syscallKill = oldKill })
	require.NoError(t, signalProcessGroupID(0, syscall.SIGTERM))
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, signalProcessGroupID(10, syscall.SIGTERM))
	syscallKill = func(int, syscall.Signal) error { return errors.New("signal") }
	require.Error(t, signalProcessGroupID(10, syscall.SIGTERM))
	syscallKill = func(int, syscall.Signal) error { return nil }
	require.NoError(t, signalProcessGroupID(10, syscall.SIGTERM))
}

func TestProcessTransportQuiescenceProofFailures(t *testing.T) {
	oldKill := syscallKill
	oldClose := processContainmentClose
	t.Cleanup(func() {
		syscallKill = oldKill
		processContainmentClose = oldClose
	})

	read, write, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	quiesced := 0
	var inventories []func() (int, bool)
	transport := &ProcessTransport{
		tree: &processContainment{processGroupID: 123, control: write, proof: supervisorProofFile(t)},
		options: Options{
			ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
				inventories = append(inventories, inventory)
			},
			ObserveProcessQuiesced: func(context.Context) { quiesced++ },
		},
	}
	syscallKill = func(int, syscall.Signal) error { return errors.New("probe failed") }
	require.ErrorIs(t, transport.quiesceProcessTree(), ErrProcessContainmentIncomplete)
	require.Zero(t, quiesced)
	require.Len(t, inventories, 1)
	_, exact := inventories[0]()
	require.False(t, exact)

	read, write, err = os.Pipe()
	require.NoError(t, err)
	require.NoError(t, read.Close())
	transport.tree = &processContainment{processGroupID: 124, control: write, proof: supervisorProofFile(t)}
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	processContainmentClose = func(*processContainment) error { return errors.New("close failed") }
	require.ErrorContains(t, transport.quiesceProcessTree(), "close Claude process containment")
	require.Equal(t, 1, quiesced)
}

func supervisorProofFile(t *testing.T) *os.File {
	t.Helper()
	read, write, err := os.Pipe()
	require.NoError(t, err)
	_, err = write.WriteString(turnSupervisorProof)
	require.NoError(t, err)
	require.NoError(t, write.Close())

	return read
}

func TestLinuxSupervisorControlEOFContainsDetachedDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	sentinel := filepath.Join(dir, "sentinel")
	native := supervisorDetachedNativeCommand(t, pidFile, sentinel)
	configureProcessCommand(native)
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{})
	require.NoError(t, err)
	tree, err := startContainedProcess(launch)
	require.NoError(t, err)

	childPID := awaitSupervisorDetachedChild(t, pidFile)
	tree.mu.Lock()
	require.NoError(t, tree.control.Close())
	tree.control = nil
	proof := tree.proof
	tree.proof = nil
	tree.mu.Unlock()
	require.NoError(t, awaitProcessTreeProof(proof, 5*time.Second))
	require.Error(t, launch.cmd.Wait(), "contained SIGKILL is an expected native exit")
	require.False(t, processExists(childPID))
	time.Sleep(800 * time.Millisecond)
	require.NoFileExists(t, sentinel)
}

func TestLinuxSupervisorDeathCannotCertifyDetachedDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	sentinel := filepath.Join(dir, "sentinel")
	transport := NewProcessTransport(nil, Options{
		CLIPath: supervisorDetachedNativePath(t),
		Env: map[string]string{
			"CLAUDE_SUPERVISOR_CHILD_PID": pidFile,
			"CLAUDE_SUPERVISOR_SENTINEL":  sentinel,
			"CLAUDE_SUPERVISOR_TEST_EXE":  mustTestExecutable(t),
		},
	})
	require.NoError(t, transport.Start(t.Context()))
	childPID := awaitSupervisorDetachedChild(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	require.NoError(t, transport.cmd.Process.Kill())
	err := transport.Close()
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.True(t, processExists(childPID),
		"unexpected supervisor death fixture must retain the escaped process so a false proof is observable")
}

func supervisorDetachedNativeCommand(t *testing.T, pidFile string, sentinel string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(supervisorDetachedNativePath(t))
	cmd.Env = append(os.Environ(),
		"CLAUDE_SUPERVISOR_CHILD_PID="+pidFile,
		"CLAUDE_SUPERVISOR_SENTINEL="+sentinel,
		"CLAUDE_SUPERVISOR_TEST_EXE="+mustTestExecutable(t),
	)

	return cmd
}

func supervisorDetachedNativePath(t *testing.T) string {
	t.Helper()

	return writeShellScript(t, filepath.Join(t.TempDir(), "claude"), `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2.1.210 (Claude Code)'; exit 0; fi
"$CLAUDE_SUPERVISOR_TEST_EXE" -test.run '^TestSupervisorDetachedChildProcess$' -- --claude-supervisor-detached-child "$CLAUDE_SUPERVISOR_CHILD_PID" "$CLAUDE_SUPERVISOR_SENTINEL" &
while :; do sleep 1; done
`)
}

func mustTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)

	return executable
}

func awaitSupervisorDetachedChild(t *testing.T, pidFile string) int {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := os.Stat(pidFile)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	require.True(t, processExists(pid))

	return pid
}

func TestSupervisorDetachedChildProcess(t *testing.T) {
	args := os.Args
	separator := -1
	for index, arg := range args {
		if arg == "--" {
			separator = index

			break
		}
	}
	if separator < 0 || len(args) != separator+4 || args[separator+1] != supervisorDetachedChildArg {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		os.Exit(2)
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	if err := os.WriteFile(args[separator+2], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(750 * time.Millisecond)
	_ = os.WriteFile(args[separator+3], []byte("escaped"), 0o600)
	select {}
}

func TestProcessTransportCloseKillsUnixProcessGroup(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 2 * time.Second
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
