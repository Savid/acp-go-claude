package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"
)

func TestMalformedStoreRecordFailsRestore(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	main := acpcore.SessionKey{SessionID: string(session.SessionId)}
	require.NoError(t, store.Replace(t.Context(), main, []acpcore.SessionStoreReplacement{
		{Key: main, Entries: rows},
		{Key: acpcore.SessionKey{SessionID: main.SessionID, Subpath: "config"}, Entries: []acpcore.SessionStoreEntry{[]byte(`{"sessionId":"wrong"}`)}},
	}))
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, t.TempDir()))
	require.Equal(t, "claude_restore_failed", requestErrorData(t, err)["error"])
}

type mirrorFaultStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *mirrorFaultStore) Replace(ctx context.Context, key acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("mirror unavailable")
	}

	return s.SessionStore.Replace(ctx, key, replacements)
}

func TestMirrorFailureFencesTurnAndAllowsRetry(t *testing.T) {
	t.Parallel()
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize(withLifecycle())
	session := h.newSession()
	store.fail.Store(true)
	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
	types := eventTypes(lifecycleEvents(h.rec.snapshot()))
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running"}, types)
	store.fail.Store(false)
	_, err = h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	types = eventTypes(lifecycleEvents(h.rec.snapshot()))
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, types)
}

func TestLoadReplayResumeAndDelete(t *testing.T) {
	t.Parallel()
	home, cwd := t.TempDir(), t.TempDir()
	h := newHarness(t, WithHome(home))
	h.initialize()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "stored", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	before := len(h.rec.snapshot())
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Equal(t, "reply: stored", agentText(h.rec.snapshot()[before:]))
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	before = len(h.rec.snapshot())
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Empty(t, agentText(h.rec.snapshot()[before:]))
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
	canonical, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	require.FileExists(t, claude.SessionPath(home, canonical, string(session.SessionId)))
}

func TestNativeFilesRequireStoreAuthority(t *testing.T) {
	t.Parallel()
	home, cwd := t.TempDir(), t.TempDir()
	first := newHarness(t, WithHome(home))
	first.initialize()
	session, err := first.conn.NewSession(first.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = first.prompt(session.SessionId, "stored", nil)
	require.NoError(t, err)
	_, err = first.conn.CloseSession(first.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	second := newHarness(t, WithHome(home))
	second.initialize()
	list, err := second.conn.ListSessions(second.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
	_, err = second.conn.LoadSession(second.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

type blockedLoadStore struct {
	acpcore.SessionStore
	block            atomic.Bool
	entered, release chan struct{}
}

func (s *blockedLoadStore) Load(ctx context.Context, key acpcore.SessionKey) ([]acpcore.SessionStoreEntry, error) {
	rows, err := s.SessionStore.Load(ctx, key)
	if err == nil && key.Subpath == "config" && s.block.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return rows, err
}
func TestDeleteWinsAgainstPreparedLoad(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "stored", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, loadErr := h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, t.TempDir()))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-h.ctx().Done():
		t.Fatal("load did not reach stored configuration")
	}
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	close(store.release)
	require.NoError(t, err)
	require.Equal(t, "unknown session", requestErrorData(t, <-done)["error"])
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
}

func TestDivergentNativeRowsRefuseRestore(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "stored", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	records, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[0], &record))
	require.NoError(t, os.WriteFile(record.SessionFile, []byte(`{"type":"user","message":{"content":"different"}}`+"\n"), 0600))
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, record.Cwd))
	require.Equal(t, "claude_restore_failed", requestErrorData(t, err)["error"])
}

func TestCloseCommitsAfterMirrorFailureWithoutReopeningStream(t *testing.T) {
	t.Parallel()
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize(withLifecycle())
	session := h.newSession()
	store.fail.Store(true)
	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
	events := lifecycleEvents(h.rec.snapshot())
	store.fail.Store(false)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	require.Equal(t, events, lifecycleEvents(h.rec.snapshot()))
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestConcurrentColdRestoreIsRefusedBeforeBinding(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach configuration")
	}
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd))
	close(store.release)
	require.Equal(t, "session_restore", requestErrorData(t, err)["limit"])
	require.NoError(t, <-done)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
}

func TestLiveResumeAppliesOptionsAndRetainsDirectories(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd, directory := t.TempDir(), t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd, WithSessionAdditionalDirectories(directory)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(session.SessionId, cwd,
		WithSessionClaudeOptions(NewClaudeOptions(WithClaudeModel("sonnet")))))
	require.NoError(t, err)
	rows, err := store.Load(h.ctx(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, "sonnet", record.Model)
	require.Equal(t, []string{directory}, record.AdditionalDirectories)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
}

func TestRestoreIntoDifferentNativeHome(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	home := t.TempDir()
	restored := newHarness(t, WithSessionStore(store), WithHome(home))
	restored.initialize()
	_, err = restored.conn.LoadSession(restored.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	_, err = restored.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	rows, err := store.Load(restored.ctx(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	var record sessionRecord
	require.Len(t, rows, 1)
	require.NoError(t, json.Unmarshal(rows[0], &record))
	relative, err := filepath.Rel(home, record.SessionFile)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relative))
	require.FileExists(t, record.SessionFile)
}
