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

func TestInMemorySessionStoreReplaceEmptyEntrySurvives(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &InMemorySessionStore{}
	main := SessionKey{SessionID: "s"}
	sub := SessionKey{SessionID: "s", Subpath: "sub/a.jsonl"}

	require.NoError(t, store.Append(ctx, main, []SessionStoreEntry{[]byte(`{"a":1}`)}))
	require.NoError(t, store.Append(ctx, sub, []SessionStoreEntry{[]byte(`{"b":2}`)}))

	// Replace lists the main key plus a subkey with empty (len==0) Entries.
	// Both survive as live, non-tombstoned keys; only unlisted keys tombstone.
	require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{}},
		{Key: sub, Entries: []SessionStoreEntry{}},
	}))

	entries, err := store.Load(ctx, main)
	require.NoError(t, err)
	require.Empty(t, entries)

	subEntries, err := store.Load(ctx, sub)
	require.NoError(t, err)
	require.Empty(t, subEntries)

	subkeys, err := store.ListSubkeys(ctx, main)
	require.NoError(t, err)
	require.Equal(t, []string{"sub/a.jsonl"}, subkeys)

	summaries, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, "s", summaries[0].SessionID)
}
