//go:build !windows

package claudeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// unremovableTestDir names a directory os.RemoveAll refuses to delete. On POSIX
// that is a child of a regular file: the path cannot be walked at all, and the
// removal reports ENOTDIR before it deletes anything.
func unremovableTestDir(t *testing.T) string {
	t.Helper()

	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(parentFile, []byte("x"), 0o600))

	return filepath.Join(parentFile, "mcp")
}
