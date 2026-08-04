//go:build linux

package claude

import (
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

func TestLinuxAgentIdentityLockSerializesAndCancels(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the trusted root supervisor identity")
	}

	uid := uint32(61000 + os.Getpid()%1000)
	first, err := acquireLinuxAgentIdentityLock(uid, strings.NewReader(""))
	require.NoError(t, err)
	defer first.Close()

	controlRead, controlWrite, err := os.Pipe()
	require.NoError(t, err)
	defer controlRead.Close()

	result := make(chan error, 1)
	go func() {
		lock, lockErr := acquireLinuxAgentIdentityLock(uid, controlRead)
		if lock != nil {
			_ = lock.Close()
		}
		result <- lockErr
	}()

	select {
	case err := <-result:
		t.Fatalf("contending identity lock completed early: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	require.NoError(t, controlWrite.Close())
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	case <-time.After(time.Second):
		t.Fatal("contending identity lock ignored closed control")
	}
}

func TestLinuxTrustedSupervisorResistsNativeAuthorityAttacks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires a privileged two-principal Linux fixture")
	}

	root, err := os.MkdirTemp("", "acp-go-claude-authority-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	require.NoError(t, os.Chmod(root, 0o777))
	resultPath := filepath.Join(root, "attacks")
	pidPath := filepath.Join(root, "daemon.pid")

	script := `
parent=$PPID
stop=denied; kill -STOP "$parent" 2>/dev/null && stop=allowed
kill_result=denied; kill -KILL "$parent" 2>/dev/null && kill_result=allowed
proof=denied; printf 'contained\n' >&6 2>/dev/null && proof=allowed
config=denied; cat /proc/$parent/fd/3 >/dev/null 2>&1 && config=allowed
printf '%s %s %s %s %s %s %s %s\n' "$stop" "$kill_result" "$proof" "$config" "$(awk '$1 == "Uid:" {print $2}' /proc/$parent/status)" "$(id -u)" "$(id -g)" "$(awk '$1 == "Groups:" {print NF-1}' /proc/self/status)" > "$1"
setsid /bin/sh -c 'trap "" TERM; while :; do sleep 1; done' >/dev/null 2>&1 &
printf '%s\n' "$!" > "$2"
`
	native := exec.Command("/bin/sh", "-c", script, "sh", resultPath, pidPath)
	native.Dir = "/"
	native.Env = []string{"PATH=/usr/bin:/bin"}
	isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"}}
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{Isolation: isolation})
	require.NoError(t, err)
	tree, err := startContainedProcess(launch)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(pidPath)

		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	rawPID, err := os.ReadFile(pidPath)
	require.NoError(t, err)
	daemonPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)

	tree.mu.Lock()
	require.NoError(t, tree.control.Close())
	tree.control = nil
	proof := tree.proof
	tree.proof = nil
	tree.mu.Unlock()
	require.NoError(t, awaitProcessTreeProof(proof, 5*time.Second))
	require.NoError(t, launch.cmd.Wait())

	result, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	fields := strings.Fields(string(result))
	require.Len(t, fields, 8)
	require.Equal(t, []string{"denied", "denied", "denied", "denied"}, fields[:4])
	require.Equal(t, "0", fields[4], "supervisor did not retain the trusted root identity")
	require.Equal(t, []string{"65534", "65534", "0"}, fields[5:], "native identity was not isolated exactly")
	require.Eventually(t, func() bool {
		err := syscall.Kill(daemonPID, 0)

		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}
