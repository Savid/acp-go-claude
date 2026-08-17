//go:build !windows

package permissions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLockPermissionsFileUsesOSLock(t *testing.T) {
	openFile := storeOpenFile
	flock := storeFlock
	t.Cleanup(func() {
		storeOpenFile = openFile
		storeFlock = flock
	})

	var calls []int
	storeFlock = func(_ int, how int) error {
		calls = append(calls, how)

		return nil
	}

	unlock, err := lockPermissionsFile(context.Background(), filepath.Join(t.TempDir(), "rules.json"))
	require.NoError(t, err)
	unlock()

	require.Equal(t, []int{unix.LOCK_EX | unix.LOCK_NB, unix.LOCK_UN}, calls)
}

func TestLockPermissionsFileOpenError(t *testing.T) {
	openFile := storeOpenFile
	t.Cleanup(func() {
		storeOpenFile = openFile
	})

	storeOpenFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("open failed")
	}

	unlock, err := lockPermissionsFile(context.Background(), filepath.Join(t.TempDir(), "rules.json"))
	require.Error(t, err)
	require.Nil(t, unlock)

	_, err = (Store{ClaudeHome: t.TempDir()}).Load(context.Background(), "session-1")
	require.Error(t, err)
}

func TestLockPermissionsFileFlockError(t *testing.T) {
	flock := storeFlock
	t.Cleanup(func() {
		storeFlock = flock
	})

	storeFlock = func(int, int) error {
		return errors.New("flock failed")
	}

	unlock, err := lockPermissionsFile(context.Background(), filepath.Join(t.TempDir(), "rules.json"))
	require.Error(t, err)
	require.Nil(t, unlock)
}

func TestLockPermissionsFileOSLockHonorsContext(t *testing.T) {
	flock := storeFlock
	t.Cleanup(func() {
		storeFlock = flock
	})

	storeFlock = func(int, int) error {
		return unix.EWOULDBLOCK
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	unlock, err := lockPermissionsFile(ctx, filepath.Join(t.TempDir(), "rules.json"))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, unlock)
}

func TestLockPermissionsFileLocalLockHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	unlock, err := lockPermissionsFile(context.Background(), path)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	blockedUnlock, err := lockPermissionsFile(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, blockedUnlock)
}

func TestLockPermissionsFileLocalLockRetriesUntilAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	unlock, err := lockPermissionsFile(context.Background(), path)
	require.NoError(t, err)

	time.AfterFunc(20*time.Millisecond, unlock)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	retryUnlock, err := lockPermissionsFile(ctx, path)
	require.NoError(t, err)
	retryUnlock()
}
