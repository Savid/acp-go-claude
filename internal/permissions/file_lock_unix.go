//go:build !windows

package permissions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var (
	storeOpenFile = os.OpenFile
	storeFlock    = unix.Flock
)

func lockOSPermissionsFile(ctx context.Context, path string) (func(), error) {
	if err := storeMkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create permission lock dir: %w", err)
	}

	file, err := storeOpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open permission lock file: %w", err)
	}

	for {
		if err := storeFlock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() {
				_ = storeFlock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		} else if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = file.Close()

			return nil, fmt.Errorf("lock permission file: %w", err)
		}

		if err := waitPermissionLockRetry(ctx); err != nil {
			_ = file.Close()

			return nil, err
		}
	}
}
