//go:build !windows

package permissions

import (
	"io"
	"os"
)

var storeOpenDir = func(dir string) (syncCloser, error) {
	return os.Open(dir)
}

type syncCloser interface {
	io.Closer
	Sync() error
}

// syncPermissionsDir flushes the directory entry, so a rename Save has already
// returned from survives a crash rather than only a clean process exit.
func syncPermissionsDir(dir string) error {
	opened, err := storeOpenDir(dir)
	if err != nil {
		return err
	}
	defer opened.Close()

	return opened.Sync()
}
