package transcript

import (
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// absTestPath builds a host-absolute path from POSIX-looking segments, so a
// test states "an absolute working directory" rather than a spelling only one
// platform accepts.
func absTestPath(segments ...string) string {
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}

	return filepath.Join(append([]string{root}, segments...)...)
}

// jsonPath renders a host path as a JSON string literal, separators included, so
// a raw transcript fixture carries a Windows path without its separators being
// read as escape sequences.
func jsonPath(path string) string {
	return strconv.Quote(path)
}

// requireProjectsPathFault asserts what the store reports when the projects path
// is a regular file rather than a directory. POSIX resolves it to ENOTDIR and
// the store reports the read failure; Windows resolves a non-directory on a
// directory path as absent, which the store answers as no stored transcripts
// rather than as a failure.
func requireProjectsPathFault(t *testing.T, err error) {
	t.Helper()

	if runtime.GOOS == "windows" {
		require.NoError(t, err)

		return
	}

	require.Error(t, err)
}
