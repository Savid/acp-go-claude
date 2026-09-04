//go:build !windows

package permissions

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSyncPermissionsDirUnix proves the POSIX flush both reaches a real
// directory and reports the two ways it can fail, which is the branch Save
// turns into a "sync permission rules dir" error.
func TestSyncPermissionsDirUnix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syncPermissionsDir(root))

	openDir := storeOpenDir
	t.Cleanup(func() { storeOpenDir = openDir })

	openErr := errors.New("open dir failed")
	storeOpenDir = func(string) (syncCloser, error) { return nil, openErr }
	require.ErrorIs(t, syncPermissionsDir(filepath.Join(root, "missing")), openErr)

	syncErr := errors.New("sync dir failed")
	storeOpenDir = func(string) (syncCloser, error) { return fakeSyncCloser{syncErr: syncErr}, nil }
	require.ErrorIs(t, syncPermissionsDir(root), syncErr)
}

type fakeSyncCloser struct {
	syncErr error
}

func (f fakeSyncCloser) Close() error {
	return nil
}

func (f fakeSyncCloser) Sync() error {
	return f.syncErr
}
