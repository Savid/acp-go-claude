//go:build unix

package claude

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	authEscapedStdoutRoleEnv   = "ACP_GO_CLAUDE_FAKE_MODE"
	authEscapedStdoutPIDEnv    = "ACP_GO_CLAUDE_FAKE_DESCENDANT_PID_FILE"
	authEscapedStdoutBinaryEnv = "ACP_GO_CLAUDE_FAKE_HELPER"
)

func TestAuthLoginFailedPresentationClosesEscapedStdout(t *testing.T) {
	if os.Getenv(authEscapedStdoutRoleEnv) == "holder" {
		_, err := syscall.Setsid()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			os.Getenv(authEscapedStdoutPIDEnv), []byte(strconv.Itoa(os.Getpid())), 0o600,
		))
		time.Sleep(30 * time.Second)

		return
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "holder.pid")
	t.Cleanup(func() { killEscapedStdoutHolder(pidFile) })

	script := "#!/bin/sh\n" +
		`"$` + authEscapedStdoutBinaryEnv + `" -test.run=^TestAuthLoginFailedPresentationClosesEscapedStdout$ &` + "\n" +
		`while [ ! -f "$` + authEscapedStdoutPIDEnv + `" ]; do sleep 0.01; done` + "\n" +
		"printf 'unclassifiable login output\\n'\n"
	options := Options{
		CLIPath:             writeShellScript(t, filepath.Join(dir, "failed-presentation"), script),
		Cwd:                 dir,
		ScratchParent:       dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
		Env: map[string]string{
			authEscapedStdoutRoleEnv:   "holder",
			authEscapedStdoutPIDEnv:    pidFile,
			authEscapedStdoutBinaryEnv: os.Args[0],
		},
	}

	started := time.Now()
	_, _, err := StartAuthLogin(t.Context(), options, nil)
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
	require.Less(t, time.Since(started), 2*time.Second,
		"failed presentation must not wait for the escaped stdout holder")

	contents, readErr := os.ReadFile(pidFile)
	require.NoError(t, readErr)
	require.NotEmpty(t, strings.TrimSpace(string(contents)))

	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
	require.NoError(t, parseErr)
	holder, findErr := os.FindProcess(pid)
	require.NoError(t, findErr)
	require.NoError(t, holder.Signal(syscall.Signal(0)),
		"escaped holder must outlive the original process-group boundary")
}
