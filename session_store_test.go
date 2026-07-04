package claudeacp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInMemorySessionStoreBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var nilStore *InMemorySessionStore
	require.Error(t, nilStore.Append(ctx, SessionKey{SessionID: "s"}, []SessionStoreEntry{[]byte(`{}`)}))
	_, err := nilStore.Load(ctx, SessionKey{SessionID: "s"})
	require.Error(t, err)
	require.Error(t, nilStore.Replace(ctx, SessionKey{SessionID: "s"}, nil))
	require.Error(t, nilStore.Delete(ctx, SessionKey{}))
	_, err = nilStore.ListSessions(ctx)
	require.Error(t, err)
	_, err = nilStore.ListSubkeys(ctx, SessionKey{SessionID: "s"})
	require.Error(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	store := &InMemorySessionStore{}
	require.ErrorIs(t, store.Append(cancelled, SessionKey{SessionID: "s"}, nil), context.Canceled)
	_, err = store.Load(cancelled, SessionKey{SessionID: "s"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.Replace(cancelled, SessionKey{SessionID: "s"}, nil), context.Canceled)
	require.ErrorIs(t, store.Delete(cancelled, SessionKey{SessionID: "s"}), context.Canceled)
	_, err = store.ListSessions(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.ListSubkeys(cancelled, SessionKey{SessionID: "s"})
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "s"}, nil))
	require.Error(t, store.Append(ctx, SessionKey{}, []SessionStoreEntry{[]byte(`{"a":1}`)}))
	require.NoError(t, store.Delete(ctx, SessionKey{}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "s"}, []SessionStoreEntry{[]byte(`{"a":1}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "other"}, []SessionStoreEntry{[]byte(`{"z":0}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "s", Subpath: "sub/a.jsonl"}, []SessionStoreEntry{[]byte(`{"b":2}`)}))

	entries, err := store.Load(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1}`, string(entries[0]))
	entries[0][0] = '['
	reloaded, err := store.Load(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1}`, string(reloaded[0]))

	subkeys, err := store.ListSubkeys(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.Equal(t, []string{"sub/a.jsonl"}, subkeys)

	summaries, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	require.Equal(t, "other", summaries[0].SessionID)

	require.Error(t, store.Replace(ctx, SessionKey{}, nil))
	require.Error(t, store.Replace(ctx, SessionKey{SessionID: "s", Subpath: "sub"}, nil))
	require.Error(t, store.Replace(ctx, SessionKey{SessionID: "s"}, []SessionStoreReplacement{{Key: SessionKey{SessionID: "other"}}}))
	require.Error(t, store.Replace(ctx, SessionKey{SessionID: "s"}, nil))
	require.Error(t, store.Replace(ctx, SessionKey{SessionID: "s"}, []SessionStoreReplacement{
		{Key: SessionKey{SessionID: "s"}},
		{Key: SessionKey{SessionID: "s"}},
	}))
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "s"}, []SessionStoreReplacement{
		{Key: SessionKey{SessionID: "s"}, Entries: []SessionStoreEntry{[]byte(`{"c":3}`)}},
		{Key: SessionKey{SessionID: "s", Subpath: "sub/b.jsonl"}, Entries: []SessionStoreEntry{nil}},
	}))
	subkeys, err = store.ListSubkeys(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.Equal(t, []string{"sub/b.jsonl"}, subkeys)

	require.NoError(t, store.Delete(ctx, SessionKey{SessionID: "s", Subpath: "sub/b.jsonl"}))
	subkeys, err = store.ListSubkeys(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.Empty(t, subkeys)

	require.NoError(t, store.Delete(ctx, SessionKey{SessionID: "s"}))
	entries, err = store.Load(ctx, SessionKey{SessionID: "s"})
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: "s", Subpath: "sub/c.jsonl"}, []SessionStoreEntry{[]byte(`{}`)}))
	entries, err = store.Load(ctx, SessionKey{SessionID: "s", Subpath: "sub/c.jsonl"})
	require.NoError(t, err)
	require.Empty(t, entries)

	require.Nil(t, cloneStoreEntries(nil))
	require.Nil(t, cloneStoreEntry(nil))
}

func TestSessionStorePathHelpers(t *testing.T) {
	t.Parallel()

	require.False(t, validUUIDShape("short"))
	require.False(t, validUUIDShape("11111111_1111-4111-8111-111111111111"))
	require.False(t, validUUIDShape("11111111-1111-4111-8111-11111111111g"))
	require.True(t, validUUIDShape("11111111-1111-4111-8111-111111111111"))
	require.True(t, validUUIDShape("AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"))

	for _, subpath := range []string{"", "/abs", `\abs`, "a\x00b", "a/./b", "a/../b", "a:/b"} {
		require.False(t, isSafeSessionSubpath(subpath), subpath)
	}
	require.True(t, isSafeSessionSubpath("sub/path.jsonl"))

	_, err := projectKeyForDirectory("")
	require.ErrorContains(t, err, "cwd is required")
	_, err = projectKeyForDirectory("relative")
	require.ErrorContains(t, err, "absolute")
	key, err := projectKeyForDirectory(t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, key)
	require.Equal(t, "-", sanitizeSessionProjectPath(""))
	require.Equal(t, "-tmp-project-1", sanitizeSessionProjectPath("/tmp/project_1"))
}
