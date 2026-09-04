//go:build !windows

package claude

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireNativeStartReportsALostWorkingDirectory drives the branch where the
// process cannot learn where it is. Deleting the directory a process is sitting
// in is a POSIX capability: Windows holds the working directory open and refuses
// to unlink it, so there is no way to reach the branch from a Windows test.
func requireNativeStartReportsALostWorkingDirectory(t *testing.T) {
	t.Helper()

	previousDirectory, err := os.Getwd()
	require.NoError(t, err)

	deletedDirectory := t.TempDir()
	require.NoError(t, os.Chdir(deletedDirectory))
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })
	require.NoError(t, os.Remove(deletedDirectory))

	_, err = startNative(t.Context(), Options{PreparedEnvironment: []string{residualSearchPath}}, residualResolvedExecutable, nil)
	require.ErrorContains(t, err, "working directory")
	require.NoError(t, os.Chdir(previousDirectory))
}
