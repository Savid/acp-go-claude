package permissions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreLoadSaveAndDelete(t *testing.T) {
	t.Parallel()

	store := Store{ClaudeHome: t.TempDir()}

	rules, err := store.Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Empty(t, rules)

	require.NoError(t, store.Save(context.Background(), "session-1", map[string]string{"Read": "allow"}))

	rules, err = store.Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": "allow"}, rules)

	rules["Read"] = "deny"
	rules, err = store.Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": "allow"}, rules)

	require.NoError(t, store.Save(context.Background(), "session-1", nil))

	rules, err = store.Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Empty(t, rules)
}

func TestStoreConcurrentWriters(t *testing.T) {
	t.Parallel()

	store := Store{ClaudeHome: t.TempDir()}
	const writers = 8

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			sessionID := fmt.Sprintf("session-%d", i)
			require.NoError(t, store.Save(context.Background(), sessionID, map[string]string{"Tool": sessionID}))
		})
	}
	wg.Wait()

	for i := range writers {
		sessionID := fmt.Sprintf("session-%d", i)
		rules, err := store.Load(context.Background(), sessionID)
		require.NoError(t, err)
		require.Equal(t, map[string]string{"Tool": sessionID}, rules)
	}
}

func TestPermissionLockRetryDelayJitters(t *testing.T) {
	t.Parallel()

	const samples = 32

	var previous time.Duration
	var sawDifferentConsecutive bool

	for i := range samples {
		delay := permissionLockRetryDelay()
		require.GreaterOrEqual(t, delay, permissionLockRetryBase)
		require.LessOrEqual(t, delay, permissionLockRetryBase+permissionLockRetryJitter)

		if i > 0 && delay != previous {
			sawDifferentConsecutive = true
		}

		previous = delay
	}

	require.True(t, sawDifferentConsecutive)
}

func TestStoreErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (Store{ClaudeHome: t.TempDir()}).Load(ctx, "session-1")
	require.ErrorIs(t, err, context.Canceled)

	err = (Store{ClaudeHome: t.TempDir()}).Save(ctx, "session-1", map[string]string{"Read": "allow"})
	require.ErrorIs(t, err, context.Canceled)

	err = (Store{ClaudeHome: t.TempDir()}).Save(context.Background(), "", map[string]string{"Read": "allow"})
	require.Error(t, err)

	home := t.TempDir()
	storeDir := filepath.Join(home, "acp-go-claude")
	storePath := filepath.Join(storeDir, "session-permissions.json")
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	require.NoError(t, os.WriteFile(storePath, []byte("{bad"), 0o600))

	rules, err := (Store{ClaudeHome: home}).Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Empty(t, rules)
	_, err = os.Stat(storePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(storePath + ".bak")
	require.NoError(t, err)
	require.Equal(t, []byte("{bad"), data)
	require.NoError(t, (Store{ClaudeHome: home}).Save(context.Background(), "session-1", map[string]string{"Read": "allow"}))

	readErrHome := t.TempDir()
	readErrDir := filepath.Join(readErrHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(readErrDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(readErrDir, "session-permissions.json"), 0o700))

	_, err = (Store{ClaudeHome: readErrHome}).Load(context.Background(), "session-1")
	require.Error(t, err)

	err = (Store{ClaudeHome: readErrHome}).Save(context.Background(), "session-1", map[string]string{"Read": "allow"})
	require.Error(t, err)

	blockingFile := filepath.Join(t.TempDir(), "claude-home")
	require.NoError(t, os.WriteFile(blockingFile, []byte("file"), 0o600))

	err = (Store{ClaudeHome: blockingFile}).Save(context.Background(), "session-1", map[string]string{"Read": "allow"})
	require.Error(t, err)
}

func TestStoreWriteAllErrorBranches(t *testing.T) {
	mkdirAll := storeMkdirAll
	marshalIndent := storeMarshalIndent
	createTemp := storeCreateTemp
	syncDir := storeSyncDir
	rename := storeRename
	userHomeDir := storeUserHomeDir
	t.Cleanup(func() {
		storeMkdirAll = mkdirAll
		storeMarshalIndent = marshalIndent
		storeCreateTemp = createTemp
		storeSyncDir = syncDir
		storeRename = rename
		storeUserHomeDir = userHomeDir
	})

	store := Store{ClaudeHome: t.TempDir()}
	rules := map[string]map[string]string{"session-1": {"Read": "allow"}}

	storeMkdirAll = func(string, os.FileMode) error {
		return errors.New("mkdir failed")
	}
	require.Error(t, store.writeAll(rules))
	storeMkdirAll = mkdirAll

	storeMarshalIndent = func(any, string, string) ([]byte, error) {
		return nil, errors.New("marshal failed")
	}
	require.Error(t, store.writeAll(rules))
	storeMarshalIndent = marshalIndent

	storeCreateTemp = func(string, string) (tempFile, error) {
		return nil, errors.New("create temp failed")
	}
	require.Error(t, store.writeAll(rules))
	storeCreateTemp = createTemp

	storeCreateTemp = func(string, string) (tempFile, error) {
		return fakeTempFile{name: filepath.Join(t.TempDir(), "tmp"), writeErr: errors.New("write failed")}, nil
	}
	require.Error(t, store.writeAll(rules))

	storeCreateTemp = func(string, string) (tempFile, error) {
		return fakeTempFile{name: filepath.Join(t.TempDir(), "tmp"), closeErr: errors.New("close failed")}, nil
	}
	require.Error(t, store.writeAll(rules))

	storeCreateTemp = func(string, string) (tempFile, error) {
		return fakeTempFile{name: filepath.Join(t.TempDir(), "tmp"), syncErr: errors.New("sync failed")}, nil
	}
	require.Error(t, store.writeAll(rules))
	storeCreateTemp = createTemp

	storeRename = func(string, string) error {
		return errors.New("rename failed")
	}
	require.Error(t, store.writeAll(rules))
	storeRename = rename

	corruptHome := t.TempDir()
	corruptStoreDir := filepath.Join(corruptHome, "acp-go-claude")
	require.NoError(t, os.MkdirAll(corruptStoreDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(corruptStoreDir, "session-permissions.json"), []byte("{bad"), 0o600))
	storeRename = func(string, string) error {
		return errors.New("backup failed")
	}
	_, err := (Store{ClaudeHome: corruptHome}).Load(context.Background(), "session-1")
	require.ErrorContains(t, err, "backup corrupt permission rules")
	storeRename = rename

	storeSyncDir = func(string) error {
		return errors.New("sync dir failed")
	}
	require.Error(t, store.writeAll(rules))
	storeSyncDir = syncDir

	storeUserHomeDir = func() (string, error) {
		return "", errors.New("home failed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.Equal(t, filepath.Clean(".claude"), Store{}.configHome())
}

type fakeTempFile struct {
	name     string
	writeErr error
	syncErr  error
	closeErr error
}

func (f fakeTempFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	return len(data), nil
}

func (f fakeTempFile) Close() error {
	return f.closeErr
}

func (f fakeTempFile) Sync() error {
	return f.syncErr
}

func (f fakeTempFile) Name() string {
	return f.name
}

func TestStoreConfigHomeAndNullFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "from-env"))

	store := Store{}
	require.Equal(t, filepath.Clean(os.Getenv("CLAUDE_CONFIG_DIR")), store.configHome())

	home := t.TempDir()
	storeDir := filepath.Join(home, "acp-go-claude")
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, "session-permissions.json"), []byte("null"), 0o600))

	rules, err := (Store{ClaudeHome: home}).Load(context.Background(), "session-1")
	require.NoError(t, err)
	require.Empty(t, rules)

	if runtime.GOOS != "windows" {
		homeDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		t.Setenv("HOME", homeDir)

		require.Equal(t, filepath.Join(homeDir, ".claude"), Store{}.configHome())
	}
}

func TestClone(t *testing.T) {
	t.Parallel()

	rules := map[string]string{"Read": "allow"}
	clone := Clone(rules)
	clone["Read"] = "deny"

	require.Equal(t, map[string]string{"Read": "allow"}, rules)
	require.Empty(t, Clone(nil))
}
