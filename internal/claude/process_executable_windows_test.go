//go:build windows

package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsOrdinaryExecutableLookupUsesOnlyTheStaticEnvironment(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude.exe"), []byte("exe"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "claude.cmd"), []byte("cmd"), 0o600))

	t.Setenv("PATHEXT", ".EXE")

	resolved, err := resolveOrdinaryExecutable("claude", []string{
		"Path=" + bin,
		"PathExt=.CMD",
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
