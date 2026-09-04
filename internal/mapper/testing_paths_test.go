package mapper

import (
	"path/filepath"
	"runtime"
	"strings"
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

// fileTestURI spells a host path as the file URI a host on this platform sends:
// a POSIX path is already the URI path, while a Windows path needs its
// separators turned around and an empty authority in front of the drive.
func fileTestURI(path string) string {
	return "file://" + fileTestURIPath(path)
}

// fileTestURIPath is the path component of that URI on its own, for a test that
// spells the authority itself.
func fileTestURIPath(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}

	return slashed
}
