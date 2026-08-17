//go:build !windows

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testBrowserProbeURL is the address the fake harness hands a launcher. It
// resolves nowhere, so nothing reaches the network even if a probe misfires.
const testBrowserProbeURL = "https://example.invalid/"

// TestLoginNeverExecsABrowserLauncher is the whole point of the shim: a login
// child that execs a launcher off PATH must reach a no-op, not a browser. Every
// name the shim shadows is probed, because a desktop only has to answer one of
// them for the grant to complete. The probe directory records every execution,
// and the positive control proves the recorder works before the absence of a
// record is allowed to mean anything.
func TestLoginNeverExecsABrowserLauncher(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	dir := testTraversableTempDir(t)
	probe := filepath.Join(dir, "probe")
	marker := filepath.Join(dir, "launched")

	require.NoError(t, os.MkdirAll(probe, 0o700))

	for _, name := range browserLauncherNames {
		writeShellScript(t, filepath.Join(probe, name), "#!/bin/sh\necho \"$0 $*\" >> "+marker+"\nexit 0\n")
	}

	t.Setenv(envSearchPath, probe+string(os.PathListSeparator)+os.Getenv(envSearchPath))

	for _, name := range browserLauncherNames {
		control := exec.CommandContext(t.Context(), name, testBrowserProbeURL)
		require.NoError(t, control.Run())
	}

	recorded, err := os.ReadFile(marker)
	require.NoError(t, err)

	for _, name := range browserLauncherNames {
		require.Contains(t, string(recorded), name+" "+testBrowserProbeURL)
	}

	require.NoError(t, os.Remove(marker))

	launches := make([]string, 0, len(browserLauncherNames))
	for _, name := range browserLauncherNames {
		launches = append(launches, name+" \""+testBrowserProbeURL+"\"")
	}

	script := "#!/bin/sh\n" +
		strings.Join(launches, "\n") + "\n" +
		"printf '%s\\n' '" + testAuthorizeURL + "'\n" +
		"printf '" + AuthLoginPrompt + "'\n" +
		"sleep 30\n"

	options, generation := authTestOptions(t, Options{Cwd: dir})
	options.CLIPath = writeShellScript(t, filepath.Join(dir, "login"), script)

	login, authorizeURL, err := StartAuthLogin(t.Context(), options, generation)
	require.NoError(t, err)
	require.Equal(t, testAuthorizeURL, authorizeURL)

	require.NoFileExists(t, marker)

	require.NoError(t, login.Close())
	require.NoFileExists(t, marker)

	entries, err := os.ReadDir(options.ScratchParent)
	require.NoError(t, err)

	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), browserShimPrefix))
	}
}

func TestNewBrowserShimFailsWhenItsDirectoryCannotBeCreated(t *testing.T) {
	original := browserShimMkdirTemp

	t.Cleanup(func() { browserShimMkdirTemp = original })

	browserShimMkdirTemp = func(string, string) (string, error) { return "", errAuthTest }

	_, err := newBrowserShim(t.TempDir())
	require.ErrorIs(t, err, errAuthTest)
}

func TestNewBrowserShimFailsWhenALauncherCannotBeWritten(t *testing.T) {
	original := browserShimWriteFile

	t.Cleanup(func() { browserShimWriteFile = original })

	browserShimWriteFile = func(string, []byte, os.FileMode) error { return errAuthTest }

	parent := t.TempDir()

	_, err := newBrowserShim(parent)
	require.ErrorIs(t, err, errAuthTest)

	// A half-written shim is torn down rather than left to shadow nothing.
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStartAuthLoginFailsClosedWhenTheBrowserLaunchCannotBeContained(t *testing.T) {
	original := browserShimMkdirTemp

	t.Cleanup(func() { browserShimMkdirTemp = original })

	browserShimMkdirTemp = func(string, string) (string, error) { return "", errAuthTest }

	options, generation := authTestOptions(t, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})

	_, _, err := StartAuthLogin(t.Context(), options, generation)
	require.ErrorIs(t, err, errAuthTest)
}
