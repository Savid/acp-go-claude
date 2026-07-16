//go:build windows

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const windowsContainmentHelperArg = "--claude-windows-containment-helper"

func TestWindowsJobContainsStubbornGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(
		executable,
		"-test.run", "^TestWindowsContainmentHelper$",
		"--", windowsContainmentHelperArg, "root", pidFile,
	)
	configureProcessCommand(cmd)
	launch, err := prepareProcessTreeCommand(cmd)
	require.NoError(t, err)
	tree, err := startContainedProcess(launch)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(pidFile)

		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	grandchildPID, err := strconv.ParseUint(strings.TrimSpace(string(rawPID)), 10, 32)
	require.NoError(t, err)

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	require.NoError(t, tree.quiesce(5*time.Second))
	require.NoError(t, tree.close())
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Windows native root was not reaped after Job Object termination")
	}

	handle, openErr := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(grandchildPID),
	)
	if openErr == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("Job Object quiescence returned while the stubborn grandchild was still openable")
	}
}

func TestWindowsContainmentHelper(t *testing.T) {
	args := os.Args
	separator := -1
	for index, arg := range args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || len(args) != separator+4 || args[separator+1] != windowsContainmentHelperArg {
		return
	}

	switch args[separator+2] {
	case "root":
		child := exec.Command(
			os.Args[0],
			"-test.run", "^TestWindowsContainmentHelper$",
			"--", windowsContainmentHelperArg, "child", args[separator+3],
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(args[separator+3], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(2)
		}
		select {}
	case "child":
		select {}
	}
}
