//go:build unix

package claude

import (
	"context"
	"errors"
	"fmt"
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

const (
	escapedStdoutRoleEnv = "ACP_GO_CLAUDE_FAKE_MODE"
	escapedStdoutPIDEnv  = "ACP_GO_CLAUDE_FAKE_DESCENDANT_PID_FILE"
)

func TestContainedClaudeOutputCancellationClosesEscapedStdout(t *testing.T) {
	switch os.Getenv(escapedStdoutRoleEnv) {
	case "parent":
		startEscapedStdoutHolder(t)

		return
	case "holder":
		require.NoError(t, os.WriteFile(
			os.Getenv(escapedStdoutPIDEnv), []byte(strconv.Itoa(os.Getpid())), 0o600,
		))
		time.Sleep(30 * time.Second)

		return
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "holder.pid")
	t.Cleanup(func() { killEscapedStdoutHolder(pidFile) })

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := containedClaudeOutput(ctx, os.Args[0], []string{
		"-test.run=^TestContainedClaudeOutputCancellationClosesEscapedStdout$",
	}, Options{
		Cwd:                 dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
		Env: map[string]string{
			escapedStdoutRoleEnv: "parent",
			escapedStdoutPIDEnv:  pidFile,
		},
	}, nil, "escaped stdout holder")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second,
		"cancellation must not wait for the escaped stdout holder")

	contents, readErr := os.ReadFile(pidFile)
	require.NoError(t, readErr)
	require.NotEmpty(t, strings.TrimSpace(string(contents)))
}

func startEscapedStdoutHolder(t *testing.T) {
	t.Helper()

	command := exec.Command(os.Args[0], "-test.run=^TestContainedClaudeOutputCancellationClosesEscapedStdout$")
	command.Env = replaceTestEnvironment(os.Environ(), escapedStdoutRoleEnv, "holder")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, command.Start())

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(escapedStdoutPIDEnv)); err == nil {
			return
		}

		if !time.Now().Before(deadline) {
			t.Fatal("escaped stdout holder did not start")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func replaceTestEnvironment(environment []string, key string, value string) []string {
	prefix := key + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			replaced = append(replaced, entry)
		}
	}

	return append(replaced, prefix+value)
}

func killEscapedStdoutHolder(pidFile string) {
	contents, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil {
		return
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_, _ = fmt.Fprintf(os.Stderr, "kill escaped stdout holder %d: %v\n", pid, err)
	}
}
