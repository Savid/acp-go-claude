//go:build linux

package claudeacp

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const detachedContainmentChildArg = "--acp-go-claude-detached-child"

func startDetachedContainmentChild(
	executable string,
	stateFile string,
	triggerFile string,
	sentinelFile string,
) error {
	rootPID := os.Getpid()
	rootPGID, err := syscall.Getpgid(rootPID)
	if err != nil {
		return fmt.Errorf("get Claude root process group: %w", err)
	}
	rootSID, err := unix.Getsid(rootPID)
	if err != nil {
		return fmt.Errorf("get Claude root session: %w", err)
	}

	child := exec.Command(
		executable,
		"-test.run", "^TestClaudeDetachedContainmentChild$",
		"--", detachedContainmentChildArg,
		stateFile, triggerFile, sentinelFile,
		strconv.Itoa(rootPID), strconv.Itoa(rootPGID), strconv.Itoa(rootSID),
	)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	return child.Start()
}

func TestClaudeDetachedContainmentChild(t *testing.T) {
	args := helperArgumentsAfterSeparator(os.Args)
	if len(args) != 7 || args[0] != detachedContainmentChildArg {
		return
	}

	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	pid := os.Getpid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		os.Exit(2)
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		os.Exit(2)
	}

	state := strings.Join([]string{
		strconv.Itoa(pid),
		strconv.Itoa(pgid),
		strconv.Itoa(sid),
		args[4],
		args[5],
		args[6],
	}, " ")
	if err := os.WriteFile(args[1], []byte(state), 0o600); err != nil {
		os.Exit(2)
	}

	for {
		if _, err := os.Stat(args[2]); err == nil {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	_ = os.WriteFile(args[3], []byte("escaped"), 0o600)
	select {}
}
