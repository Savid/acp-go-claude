package claudeacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInMemorySessionStoreAndProjectKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	require.NotEmpty(t, projectKey)
	require.Equal(t, "-", sanitizeSessionProjectPath(""))

	key := SessionKey{ProjectKey: projectKey, SessionID: "00000000-0000-4000-8000-000000000000"}
	subkey := SessionKey{ProjectKey: projectKey, SessionID: key.SessionID, Subpath: "subagents/agent-1"}
	require.NoError(t, store.Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	require.NoError(t, store.Append(ctx, subkey, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))
	require.NoError(t, store.ReplaceSession(ctx, key, []SessionStoreReplacement{
		{Key: key, Entries: []SessionStoreEntry{json.RawMessage(`{"type":"replacement"}`)}},
	}))

	loaded, err := store.Load(ctx, key)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.JSONEq(t, `{"type":"replacement"}`, string(loaded[0]))
	loaded[0][0] = ' '

	loadedAgain, err := store.Load(ctx, key)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"replacement"}`, string(loadedAgain[0]))

	summaries, err := store.ListSessions(ctx, projectKey)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, key.SessionID, summaries[0].SessionID)

	subkeys, err := store.ListSubkeys(ctx, key)
	require.NoError(t, err)
	require.Empty(t, subkeys)

	require.NoError(t, store.Delete(ctx, key))
	loaded, err = store.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, loaded)
	subkeys, err = store.ListSubkeys(ctx, key)
	require.NoError(t, err)
	require.Empty(t, subkeys)
}

func TestInMemorySessionStoreZeroValueSortAndDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &InMemorySessionStore{}
	keyA := SessionKey{ProjectKey: "project", SessionID: "00000000-0000-4000-8000-000000000001"}
	keyB := SessionKey{ProjectKey: "project", SessionID: "00000000-0000-4000-8000-000000000002"}
	subA := SessionKey{ProjectKey: "project", SessionID: keyA.SessionID, Subpath: "subagents/a"}
	otherProject := SessionKey{ProjectKey: "other", SessionID: keyA.SessionID, Subpath: "subagents/a"}

	require.NoError(t, store.Append(ctx, keyA, nil))
	require.NoError(t, store.Append(ctx, keyA, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	time.Sleep(time.Millisecond)
	require.NoError(t, store.Append(ctx, keyB, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))
	require.NoError(t, store.Append(ctx, subA, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))
	require.NoError(t, store.Append(ctx, otherProject, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))

	summaries, err := store.ListSessions(ctx, "project")
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	require.Equal(t, keyB.SessionID, summaries[0].SessionID)

	store.mu.Lock()
	store.mtime[keyB] = store.mtime[keyA]
	store.mu.Unlock()

	summaries, err = store.ListSessions(ctx, "project")
	require.NoError(t, err)
	require.Equal(t, keyA.SessionID, summaries[0].SessionID)

	require.NoError(t, store.Delete(ctx, subA))
	loaded, err := store.Load(ctx, keyA)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	loaded, err = store.Load(ctx, otherProject)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	subkeys, err := store.ListSubkeys(ctx, keyA)
	require.NoError(t, err)
	require.Empty(t, subkeys)
}

func TestSessionStoreErrorsAndValidation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var store *InMemorySessionStore
	key := SessionKey{ProjectKey: "project", SessionID: "session"}
	require.Error(t, store.Append(context.Background(), key, []SessionStoreEntry{json.RawMessage(`{}`)}))
	_, err := store.Load(context.Background(), key)
	require.Error(t, err)
	_, err = store.ListSessions(context.Background(), "project")
	require.Error(t, err)
	_, err = store.ListSubkeys(context.Background(), key)
	require.Error(t, err)
	require.Error(t, store.Delete(context.Background(), key))
	require.Error(t, store.ReplaceSession(context.Background(), key, []SessionStoreReplacement{{Key: key}}))

	store = NewInMemorySessionStore()
	require.Error(t, store.Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{}`)}))
	_, err = store.Load(ctx, key)
	require.Error(t, err)
	_, err = store.ListSessions(ctx, "project")
	require.Error(t, err)
	_, err = store.ListSubkeys(ctx, key)
	require.Error(t, err)
	require.Error(t, store.Delete(ctx, key))
	require.Error(t, store.ReplaceSession(ctx, key, nil))
	require.Error(t, store.ReplaceSession(context.Background(), key, []SessionStoreReplacement{{Key: SessionKey{ProjectKey: "other", SessionID: "session"}}}))

	store = &InMemorySessionStore{}
	require.NoError(t, store.ReplaceSession(context.Background(), key, []SessionStoreReplacement{{Key: key}}))
	loaded, err := store.Load(context.Background(), key)
	require.NoError(t, err)
	require.Empty(t, loaded)

	_, err = projectKeyForDirectory("")
	require.Error(t, err)
	_, err = projectKeyForDirectory("relative")
	require.Error(t, err)
	projectKey, err := projectKeyForDirectory(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	require.NotEmpty(t, projectKey)

	dir := t.TempDir()
	canonicalDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	projectKey, err = projectKeyForDirectory(link)
	require.NoError(t, err)
	require.Equal(t, sanitizeSessionProjectPath(canonicalDir), projectKey)

	require.False(t, isSafeSessionSubpath(""))
	require.False(t, isSafeSessionSubpath("../bad"))
	require.False(t, isSafeSessionSubpath("bad/../path"))
	require.False(t, isSafeSessionSubpath("C:/bad"))
	require.True(t, isSafeSessionSubpath("subagents/agent-1"))
}
