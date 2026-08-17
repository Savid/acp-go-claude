package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Executable is one native executable admitted for launch: an absolute path
// together with the identity of the file that path named at admission. Nothing
// downstream resolves a name a second time. The version probe and the launch it
// admits both re-read this one path and refuse when the file underneath is no
// longer the admitted one, so the adapter can never validate one file and
// execute another.
type Executable struct {
	path string
	info os.FileInfo
}

// Path answers the admitted absolute path.
func (e Executable) Path() string {
	return e.path
}

// Admitted reports whether this value carries a frozen identity.
func (e Executable) Admitted() bool {
	return e.info != nil
}

// freezeExecutable records the identity of an already-resolved executable.
// Absoluteness is the load-bearing part: a launch runs with the session's
// working directory rather than the adapter's, so a relative path would be read
// against one directory and executed against another.
func freezeExecutable(path string) (Executable, error) {
	if !filepath.IsAbs(path) {
		return Executable{}, fmt.Errorf("executable path %q is not absolute", path)
	}

	canonical := filepath.Clean(path)

	info, err := os.Stat(canonical)
	if err != nil {
		return Executable{}, fmt.Errorf("stat executable %q: %w", canonical, err)
	}

	return Executable{path: canonical, info: info}, nil
}

// verify re-reads the admitted path immediately before an exec. os.SameFile
// compares the recorded device and inode identity, and the volume and file
// index on Windows, which has no fexecve either; a replaced file or a repointed
// symlink is therefore refused rather than run in place of the admitted one.
func (e Executable) verify() error {
	if e.info == nil {
		return errors.New("claude executable was never admitted")
	}

	info, err := os.Stat(e.path)
	if err != nil {
		return fmt.Errorf("verify executable %q: %w", e.path, err)
	}

	if !os.SameFile(e.info, info) {
		return fmt.Errorf("executable %q is no longer the admitted file", e.path)
	}

	return nil
}
