//go:build windows

package permissions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

var storeOpenFile = os.OpenFile

func lockOSPermissionsFile(ctx context.Context, path string) (func(), error) {
	if err := storeMkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create permission lock dir: %w", err)
	}

	file, err := storeOpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open permission lock file: %w", err)
	}

	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped

	for {
		err := windows.LockFileEx(
			handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
				_ = file.Close()
			}, nil
		}

		if err != windows.ERROR_LOCK_VIOLATION {
			_ = file.Close()

			return nil, fmt.Errorf("lock permission file: %w", err)
		}

		if err := waitPermissionLockRetry(ctx); err != nil {
			_ = file.Close()

			return nil, err
		}
	}
}
