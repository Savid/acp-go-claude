//go:build windows

package claudeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// unremovableTestDir names a directory os.RemoveAll refuses to delete. Windows
// resolves a child of a regular file as simply absent, so the POSIX shape would
// be removed without complaint; what it does refuse is unlinking a file another
// handle still holds open, which is the condition this builds instead.
func unremovableTestDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "mcp")
	require.NoError(t, os.Mkdir(dir, 0o700))

	held, err := os.Create(filepath.Join(dir, "held"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	return dir
}
