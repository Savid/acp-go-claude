//go:build windows

package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrdinaryWindowsExecutableAndEnvironmentBehavior(t *testing.T) {
	ignored := t.TempDir()
	selected := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ignored, "claude.exe"), []byte("ignored"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(selected, "claude.cmd"), []byte("@exit /b 0\r\n"), 0o600))

	resolved, err := resolveOrdinaryExecutable("claude", []string{
		"Path=" + ignored,
		"PathExt=.EXE",
		"PATH=" + selected,
		"PATHEXT=.CMD",
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(selected, "claude.cmd"), resolved)
}

func TestWindowsEnvironmentCollapsesRepeatedSpellingsDeterministically(t *testing.T) {
	environment := BuildEnv(Options{
		OrdinaryEnvironment: map[string]string{"Path": "base", "PATHEXT": ".EXE"},
		Env:                 map[string]string{"PATH": "overlay", "PathExt": ".CMD"},
	})

	values := map[string][]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[EnvironmentKey(key)] = append(values[EnvironmentKey(key)], value)
		}
	}
	require.Equal(t, []string{"overlay"}, values["PATH"])
	require.Equal(t, []string{".CMD"}, values["PATHEXT"])
}
