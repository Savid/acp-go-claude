//go:build windows

package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsOrdinaryExecutableLookupUsesOnlyTheStaticEnvironment(t *testing.T) {
	ignored := t.TempDir()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ignored, "claude.exe"), []byte("exe"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude.exe"), []byte("exe"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude.cmd"), []byte("cmd"), 0o600))

	t.Setenv("PATHEXT", ".EXE")

	resolved, err := resolveOrdinaryExecutable("claude", []string{
		"Path=" + ignored,
		"PathExt=.EXE",
		"PATH=" + bin,
		"PATHEXT=.CMD",
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bin, "claude.cmd"), resolved)
}

func TestWindowsOrdinaryExecutableLookupUsesDefaultsWhenStaticPathextIsAbsent(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude.exe"), []byte("exe"), 0o600))

	t.Setenv("PATHEXT", ".AMBIENT")

	resolved, err := resolveOrdinaryExecutable("claude", []string{"Path=" + bin})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bin, "claude.exe"), resolved)
}

func TestWindowsOrdinaryExecutableLookupMatchesBuiltEnvironmentLastWins(t *testing.T) {
	nativeBin := t.TempDir()
	overrideBin := t.TempDir()
	copyWindowsTestExecutable(t, filepath.Join(overrideBin, "claude.exe"))
	require.NoError(t, os.WriteFile(filepath.Join(nativeBin, "claude.cmd"), []byte("exit /b 0\r\n"), 0o600))

	environment := BuildEnv(Options{
		OrdinaryEnvironment: map[string]string{
			"Path":    nativeBin,
			"PathExt": ".CMD",
		},
		Env: map[string]string{
			"PATH":    overrideBin,
			"PATHEXT": ".EXE",
		},
	})
	require.Len(t, windowsEnvironmentValues(environment, "PATH"), 1)
	require.Len(t, windowsEnvironmentValues(environment, "PATHEXT"), 1)

	resolved, err := resolveOrdinaryExecutable("claude", environment)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(overrideBin, "claude.exe"), resolved)

	received := runWindowsEnvironmentChild(t, resolved, environment)
	require.Equal(t, overrideBin, received.Path)
	require.Equal(t, ".EXE", received.PathExt)
}

func TestWindowsOrdinaryExecutableLookupPreservesBaseWithExtraPathDirs(t *testing.T) {
	extraBin := t.TempDir()
	nativeBin := t.TempDir()
	copyWindowsTestExecutable(t, filepath.Join(extraBin, "claude.exe"))
	copyWindowsTestExecutable(t, filepath.Join(nativeBin, "claude.exe"))

	environment := BuildEnv(Options{
		OrdinaryEnvironment: map[string]string{
			"Path":    nativeBin,
			"PathExt": ".EXE",
		},
		ExtraPathDirs: []string{extraBin},
	})
	wantPath := strings.Join([]string{extraBin, nativeBin}, string(os.PathListSeparator))
	require.Equal(t, []string{wantPath}, windowsEnvironmentValues(environment, "PATH"))
	require.Equal(t, []string{".EXE"}, windowsEnvironmentValues(environment, "PATHEXT"))

	resolved, err := resolveOrdinaryExecutable("claude", environment)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(extraBin, "claude.exe"), resolved)

	received := runWindowsEnvironmentChild(t, resolved, environment)
	require.Equal(t, wantPath, received.Path)
	require.Equal(t, ".EXE", received.PathExt)
}

type windowsEnvironmentChildResult struct {
	Path    string `json:"path"`
	PathExt string `json:"path_ext"`
}

const windowsEnvironmentChildResultEnv = "CLAUDE_WINDOWS_ENVIRONMENT_CHILD_RESULT"

func runWindowsEnvironmentChild(
	t *testing.T,
	path string,
	environment []string,
) windowsEnvironmentChildResult {
	t.Helper()

	resultPath := filepath.Join(t.TempDir(), "environment.json")
	command := processCommand(
		path,
		"-test.run=^TestWindowsEnvironmentChildProcess$",
	)
	command.Env = append(
		append([]string(nil), environment...),
		windowsEnvironmentChildResultEnv+"="+resultPath,
	)
	require.NoError(t, command.Run())

	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)

	var result windowsEnvironmentChildResult
	require.NoError(t, json.Unmarshal(data, &result))

	return result
}

func copyWindowsTestExecutable(t *testing.T, destination string) {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(destination, data, 0o700))
}

func windowsEnvironmentValues(environment []string, key string) []string {
	values := make([]string, 0, 1)
	for _, entry := range environment {
		candidate, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(candidate, key) {
			values = append(values, value)
		}
	}

	return values
}

func TestWindowsEnvironmentChildProcess(t *testing.T) {
	resultPath := os.Getenv(windowsEnvironmentChildResultEnv)
	if resultPath == "" {
		return
	}

	result := windowsEnvironmentChildResult{
		Path:    os.Getenv("PATH"),
		PathExt: os.Getenv("PATHEXT"),
	}
	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(resultPath, data, 0o600))
}

// TestEnvironmentKeyFoldsOnWindows pins the identity half of the platform seam.
// Windows environment names are case-insensitive, so Path and PATH are one
// variable and the seam must report one identity for both.
func TestEnvironmentKeyFoldsOnWindows(t *testing.T) {
	t.Parallel()

	require.Equal(t, envSearchPath, EnvironmentKey("path"))
	require.Equal(t, EnvironmentKey(envSearchPath), EnvironmentKey("Path"))
}
