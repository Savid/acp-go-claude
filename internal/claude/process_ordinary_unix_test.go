//go:build !windows

package claude

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrdinaryExecutableResolutionPrecedesWorkingDirectoryChange(t *testing.T) {
	adapterDir := t.TempDir()
	sessionDir := t.TempDir()
	adapterExecutable := filepath.Join(adapterDir, "claude")
	sessionExecutable := filepath.Join(sessionDir, "claude")
	require.NoError(t, os.WriteFile(adapterExecutable, []byte("#!/bin/sh\nprintf adapter"), 0o700))
	require.NoError(t, os.WriteFile(sessionExecutable, []byte("#!/bin/sh\nprintf session"), 0o700))

	previous := ordinaryGetwd
	ordinaryGetwd = func() (string, error) { return adapterDir, nil }
	t.Cleanup(func() { ordinaryGetwd = previous })

	process, err := startOrdinaryNative("claude", nil, []string{"PATH=."}, sessionDir)
	require.NoError(t, err)
	require.NoError(t, process.Stdin().Close())
	output, err := io.ReadAll(process.Stdout())
	require.NoError(t, err)
	result, err := process.Wait(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.ExitCode)
	require.Equal(t, "adapter", string(output))
}

func TestOrdinaryNativeResultPreservesNaturalAndRevokedOutcomes(t *testing.T) {
	natural, err := startOrdinaryNative("/bin/sh", []string{"-c", "exit 7"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	require.NoError(t, natural.Stdin().Close())
	result, err := natural.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.Zero(t, result.Signal)
	require.False(t, result.Revoked)
	require.NoError(t, natural.Revoke(t.Context()))
	result, err = natural.Wait(t.Context())
	require.NoError(t, err)
	require.False(t, result.Revoked)

	revoked, err := startOrdinaryNative("/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	revokeErr := revoked.Revoke(cancelled)
	if revokeErr != nil {
		require.ErrorIs(t, revokeErr, context.Canceled)
	}
	result, err = revoked.Wait(t.Context())
	require.NoError(t, err)
	require.True(t, result.Revoked)
	require.Equal(t, int(syscall.SIGKILL), result.Signal)
}
