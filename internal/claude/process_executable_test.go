package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAdmittedExecutableRunsTheFileItValidatedAcrossACwdTransition is the
// deterministic proof for the resolution seam. The adapter and the session it
// launches for run in different directories, and both directories hold a file
// named claude. Discovery must answer the one the adapter can see, and the
// launch — which replaces the child's working directory with the session's —
// must run that same file rather than the one the child's directory names.
func TestAdmittedExecutableRunsTheFileItValidatedAcrossACwdTransition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts and executable mode bits")
	}

	originalGetwd := processGetwd
	t.Cleanup(func() { processGetwd = originalGetwd })

	adapterDir := t.TempDir()
	sessionDir := t.TempDir()

	writeShellScript(t, filepath.Join(adapterDir, "claude"), "#!/bin/sh\nprintf adapter-file\n")
	writeShellScript(t, filepath.Join(sessionDir, "claude"), "#!/bin/sh\nprintf session-file\n")

	processGetwd = func() (string, error) { return adapterDir, nil }

	// A "." search-path entry names two different files here: one before the
	// launch changes directory and one after.
	options := Options{
		Cwd:                 sessionDir,
		OrdinaryEnvironment: map[string]string{envSearchPath: "."},
	}

	executable, err := Discover(t.Context(), "claude", options)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(adapterDir, "claude"), executable.Path())

	output, err := containedClaudeOutput(t.Context(), executable, nil, options, nil, "identity proof")
	require.NoError(t, err)
	require.Equal(t, "adapter-file", string(output))

	// The file underneath the admitted path is replaced by a different one. The
	// path still resolves, so only the recorded identity can refuse it.
	require.NoError(t, os.Remove(filepath.Join(adapterDir, "claude")))
	writeShellScript(t, filepath.Join(adapterDir, "claude"), "#!/bin/sh\nprintf replaced-file\n")

	_, err = containedClaudeOutput(t.Context(), executable, nil, options, nil, "identity proof")
	require.ErrorContains(t, err, "no longer the admitted file")

	launchOptions := options
	launchOptions.Executable = executable

	transport := NewProcessTransport(nil, launchOptions)
	require.ErrorContains(t, transport.Start(t.Context()), "admit claude launch executable")
}

// TestExecutableAdmissionRefusesUnfrozenAndUnreadableFiles covers the identity
// seam's own refusals: nothing may be launched from a value that never went
// through admission, from a relative path, or from a path whose file is gone.
func TestExecutableAdmissionRefusesUnfrozenAndUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts and executable mode bits")
	}

	require.ErrorContains(t, Executable{}.verify(), "never admitted")
	require.False(t, Executable{}.Admitted())

	_, err := freezeExecutable(filepath.Join("relative", "claude"))
	require.ErrorContains(t, err, "is not absolute")

	_, err = freezeExecutable(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "stat executable")

	dir := t.TempDir()
	path := writeShellScript(t, filepath.Join(dir, "claude"), "#!/bin/sh\nexit 0\n")

	executable, err := freezeExecutable(path)
	require.NoError(t, err)
	require.NoError(t, executable.verify())

	require.NoError(t, os.Remove(path))
	require.ErrorContains(t, executable.verify(), "verify executable")
}

// TestClientExecutableAnswersOnlyForALaunchedProcess proves the identity a
// session carries into a relaunch comes from a real launch. An injected
// transport starts no process, so it admits nothing to reuse.
func TestClientExecutableAnswersOnlyForALaunchedProcess(t *testing.T) {
	require.False(t, NewClient(nil, Options{}, &fakeTransport{}).Executable().Admitted())

	dir := t.TempDir()
	executable := admitExecutable(t, writeShellScript(t, filepath.Join(dir, "claude"), "#!/bin/sh\nexit 0\n"))

	process := NewProcessTransport(nil, Options{})
	process.executable = executable

	require.Equal(t, executable.Path(), NewClient(nil, Options{}, process).Executable().Path())
}
