package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
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
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	main := acpcore.SessionKey{SessionID: string(session.SessionId)}
	require.NoError(t, store.Replace(t.Context(), main, []acpcore.SessionStoreReplacement{
		{Key: main, Entries: rows},
		{Key: acpcore.SessionKey{SessionID: main.SessionID, Subpath: "config"}, Entries: []acpcore.SessionStoreEntry{[]byte(`{"sessionId":"wrong"}`)}},
	}))
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, t.TempDir()))
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

func (s *mirrorFaultStore) Append(ctx context.Context, key acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("mirror unavailable")
	}

	return s.SessionStore.Append(ctx, key, base, entries, replacements)
}

func TestMirrorFailureFencesTurnAndAllowsRetry(t *testing.T) {
	t.Parallel()

	for _, prompt := range []string{"HELLO", "ERROR"} {
		t.Run(prompt, func(t *testing.T) {
			t.Parallel()

			store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
			h := newHarness(t, WithSessionStore(store))
			h.initialize(withLifecycle())
			created := h.newSession()
			before, err := store.Load(t.Context(), string(created.SessionId))
			require.NoError(t, err)
			store.fail.Store(true)
			t.Cleanup(func() { store.fail.Store(false) })
			response, err := h.prompt(created.SessionId, prompt, promptMeta(1))
			require.Empty(t, response.StopReason)
			require.Equal(t, -32603, requestErrorCode(t, err))
			data := requestErrorData(t, err)
			require.Equal(t, "claude_turn_failed", data["error"])
			if prompt == "ERROR" {
				require.Equal(t, "provider", data["cause"])
				require.Equal(t, "provider refused", data["message"])
			} else {
				require.Equal(t, "transport", data["cause"])
				require.Equal(t, "session mirror commit failed", data["message"])
			}
			after, err := store.Load(t.Context(), string(created.SessionId))
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running"}, eventTypes(lifecycleEvents(h.rec.snapshot())))

			store.fail.Store(false)
			response, err = h.prompt(created.SessionId, "HELLO", promptMeta(2))
			require.NoError(t, err)
			require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
			require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, eventTypes(lifecycleEvents(h.rec.snapshot())))
			var streams []string
			for _, update := range h.rec.snapshot() {
				envelope, ok := update.Meta[wire.LifecycleKey].(map[string]any)
				if !ok {
					continue
				}
				event, ok := envelope["event"].(map[string]any)
				if ok && event["type"] == "lifecycle_snapshot" {
					id, ok := envelope["streamId"].(string)
					require.True(t, ok)
					streams = append(streams, id)
				}
			}
			require.Len(t, streams, 2)
			require.NotEqual(t, streams[0], streams[1])
		})
	}
}

func TestLoadReplayResumeAndDelete(t *testing.T) {
	t.Parallel()
	home, cwd := t.TempDir(), t.TempDir()
	h := newHarness(t, WithHome(home))
	h.initialize()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "stored", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	before := len(h.rec.snapshot())
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Equal(t, "reply: stored", agentText(h.rec.snapshot()[before:]))
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	before = len(h.rec.snapshot())
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Empty(t, agentText(h.rec.snapshot()[before:]))
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
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
	session, err := first.conn.NewSession(first.ctx(), wire.NewSessionRequest(cwd))
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
	_, err = second.conn.LoadSession(second.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

type blockedLoadStore struct {
	acpcore.SessionStore
	block            atomic.Bool
	entered, release chan struct{}
}

func (s *blockedLoadStore) Load(ctx context.Context, sessionID string) (map[string][]acpcore.SessionStoreEntry, error) {
	rows, err := s.SessionStore.Load(ctx, sessionID)
	if err == nil && s.block.CompareAndSwap(true, false) {
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
		_, loadErr := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, t.TempDir()))
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
	records, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(records[0], &record))
	require.NoError(t, os.WriteFile(record.NativePath, []byte(`{"type":"user","message":{"content":"different"}}`+"\n"), 0600))
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, record.Cwd))
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
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestConcurrentColdRestoreIsRefusedBeforeBinding(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, wire.LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach configuration")
	}
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd))
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
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, wire.WithSessionAdditionalDirectories(directory)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd,
		WithSessionClaudeOptions(NewClaudeOptions(WithClaudeModel("sonnet")))))
	require.NoError(t, err)
	rows, err := loadEntries(h.ctx(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
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
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	home := t.TempDir()
	restored := newHarness(t, WithSessionStore(store), WithHome(home))
	restored.initialize()
	_, err = restored.conn.LoadSession(restored.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	_, err = restored.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	rows, err := loadEntries(restored.ctx(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	var record sessionRecord
	require.Len(t, rows, 1)
	require.NoError(t, json.Unmarshal(rows[0], &record))
	relative, err := filepath.Rel(home, record.NativePath)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relative))
	require.FileExists(t, record.NativePath)
}

func TestEmptySessionRestores(t *testing.T) {
	h := newHarness(t)
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
	require.NoError(t, err, "a successfully created session must remain recoverable before its first prompt")
}

func TestAgentCycleCommitFailureEndsTheGeneration(t *testing.T) {
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	gate := filepath.Join(t.TempDir(), "background")
	a := NewAgent(testOptions(t, WithSessionStore(store), WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvBackgroundGate: gate}))...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	init := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&init)
	_, err := a.Initialize(t.Context(), init)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.runtime
	s.mu.Unlock()
	request := wire.TextPromptRequest(created.SessionId, "AGENTWORK")
	request.Meta = promptMeta(1)
	_, err = a.Prompt(t.Context(), request)
	require.NoError(t, err)
	store.fail.Store(true)
	require.NoError(t, os.WriteFile(gate, nil, 0o600))

	// A failed background commit has no terminal wire event. Wait for the
	// pump to finish shutdown, then submit exactly one recovery prompt.
	select {
	case <-rt.done:
	case <-time.After(testTimeout):
		t.Fatal("failed background commit did not end its generation")
	}
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle", "state_update:running"},
		eventTypes(lifecycleEvents(rec.snapshot())), "an uncommitted agent-origin cycle must not publish a terminal idle")
	store.fail.Store(false)
	request = wire.TextPromptRequest(created.SessionId, "HELLO")
	request.Meta = promptMeta(2)
	response, err := a.Prompt(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.Equal(t, 2, lifecycleIncarnations(rec.snapshot()), "a fenced stream never reopened")
}

// Residual native state with no store entry is neither listed nor adopted.
func TestResidualNativeStateIsNeverAdopted(t *testing.T) {
	t.Parallel()

	home, cwd := t.TempDir(), t.TempDir()
	h := newHarness(t, WithHome(home))
	h.initialize()

	orphan := "00000000-0000-4000-8000-00000000abcd"
	_, err := sessionlog.WriteJSONL(claude.SessionPath(home, cwd, orphan), [][]byte{
		[]byte(`{"type":"user","uuid":"11111111-1111-4111-8111-111111111111","sessionId":"` + orphan + `","cwd":"` + cwd + `","timestamp":"2026-09-15T00:00:00Z","message":{"id":"m1","role":"user","content":[{"type":"text","text":"hello"}]}}`),
	})
	require.NoError(t, err)

	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)["error"])
}

type recoveryFaultStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *recoveryFaultStore) Replace(ctx context.Context, main acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("injected store failure")
	}

	return s.SessionStore.Replace(ctx, main, replacements)
}

func (s *recoveryFaultStore) Append(ctx context.Context, key acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("injected store failure")
	}

	return s.SessionStore.Append(ctx, key, base, entries, replacements)
}

func TestNativeBindingSurvivesLoadAndResume(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(created.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, record.NativeSessionID)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), created.Meta)
	id := acp.SessionId("acp-conversation-independent-of-native-id")
	record.SessionID = string(id)
	_, err = sessionlog.NewMirror(store, string(id)).Commit(t.Context(), sessionlog.Rows(rows), func(sessionlog.Tail) (any, error) { return record, nil })
	require.NoError(t, err)
	require.NoError(t, store.Delete(t.Context(), acpcore.SessionKey{SessionID: string(created.SessionId)}))

	before := len(h.rec.snapshot())
	loaded, err := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), loaded.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	for _, update := range h.rec.snapshot()[before:] {
		require.Equal(t, id, update.SessionId)
	}
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, id, listed.Sessions[0].SessionId)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: id})
	require.NoError(t, err)
	listed, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	resumed, err := h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, loaded.Meta, resumed.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	var after sessionRecord
	_, found, err = sessionlog.Load(t.Context(), store, string(id), &after)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record.NativeSessionID, after.NativeSessionID)
	require.Equal(t, string(id), after.SessionID)
}

type firstMirrorFailureStore struct {
	acpcore.SessionStore
	calls atomic.Int32
}

func (s *firstMirrorFailureStore) Replace(ctx context.Context, key acpcore.SessionKey, rows []acpcore.SessionStoreReplacement) error {
	if s.calls.Add(1) == 1 {
		return errors.New("initial mirror unavailable")
	}

	return s.SessionStore.Replace(ctx, key, rows)
}

func (s *firstMirrorFailureStore) Append(ctx context.Context, key acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	if s.calls.Add(1) == 1 {
		return errors.New("initial mirror unavailable")
	}

	return s.SessionStore.Append(ctx, key, base, entries, replacements)
}

func TestFailedNewSessionDoesNotPersistDuringCleanup(t *testing.T) {
	t.Parallel()
	store := &firstMirrorFailureStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	response, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, response.SessionId)
	rows, err := store.ListSessions(h.ctx())
	require.NoError(t, err)
	require.Empty(t, rows)
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
}

type firstOpenFailureClient struct {
	*recorder
	failed atomic.Bool
}

func (c *firstOpenFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if c.failed.CompareAndSwap(false, true) {
		return errors.New("initial publication unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestFailedSessionOpenReleasesActiveSlot(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(&firstOpenFailureClient{recorder: newRecorder()}, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	first, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, first.SessionId)
	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
}

// countingStore records every write the sibling makes to the store.
type countingStore struct {
	acpcore.SessionStore
	replaces atomic.Int32
	appends  atomic.Int32
	appended atomic.Int32
	deletes  atomic.Int32
}

func (s *countingStore) Replace(ctx context.Context, key acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	s.replaces.Add(1)

	return s.SessionStore.Replace(ctx, key, replacements)
}

func (s *countingStore) Append(ctx context.Context, key acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	s.appends.Add(1)
	s.appended.Add(int32(len(entries)))

	return s.SessionStore.Append(ctx, key, base, entries, replacements)
}

func (s *countingStore) Delete(ctx context.Context, key acpcore.SessionKey) error {
	s.deletes.Add(1)

	return s.SessionStore.Delete(ctx, key)
}

func ephemeralMeta() wire.SessionRequestOption {
	return wire.WithSessionMeta(wire.SessionMeta{Ephemeral: true}.Apply(nil))
}

func TestEphemeralSessionNeverReachesTheStore(t *testing.T) {
	t.Parallel()
	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession(ephemeralMeta())
	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions, "a live ephemeral session is never listed")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	require.Zero(t, store.replaces.Load())
	require.Zero(t, store.deletes.Load())
	generation, err := store.Load(h.ctx(), string(session.SessionId))
	require.NoError(t, err)
	require.Nil(t, generation)
	summaries, err := store.ListSessions(h.ctx())
	require.NoError(t, err)
	require.Empty(t, summaries)
	list, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	durable := h.newSession()
	_, err = h.prompt(durable.SessionId, "HELLO", nil)
	require.NoError(t, err)
	require.Positive(t, store.replaces.Load())
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: durable.SessionId})
	require.NoError(t, err)
	require.EqualValues(t, 1, store.deletes.Load())
}

func TestSessionMetaIsRefusedOutsideNew(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd, ephemeralMeta()))
	require.Equal(t, "_meta."+wire.SessionMetaKey, requestErrorData(t, err)["field"])
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd, ephemeralMeta()))
	require.Equal(t, "_meta."+wire.SessionMetaKey, requestErrorData(t, err)["field"])
	_, err = h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, wire.WithSessionMeta(map[string]any{
		wire.SessionMetaKey: map[string]any{"persist": false},
	})))
	require.Equal(t, "_meta."+wire.SessionMetaKey+".persist", requestErrorData(t, err)["field"])
}

// Each turn appends only the transcript rows claude added; a configuration
// change rewrites only the record, and a commit with nothing new writes
// nothing.
func TestMirrorAppendsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	created := h.newSession()
	require.EqualValues(t, 1, store.replaces.Load(), "establishment publishes the first generation")

	for turn := 1; turn <= 2; turn++ {
		_, err := h.prompt(created.SessionId, "HELLO", nil)
		require.NoError(t, err)
		require.EqualValues(t, turn, store.appends.Load())
		require.EqualValues(t, 2*turn, store.appended.Load(), "a turn appends its user and assistant rows only")
	}

	_, err := h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(created.SessionId, "haiku"))
	require.NoError(t, err)
	require.EqualValues(t, 3, store.appends.Load())
	require.EqualValues(t, 4, store.appended.Load(), "a configuration change appends no rows")

	s, err := h.agent.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	require.NoError(t, s.commitMirror(t.Context()))
	require.EqualValues(t, 3, store.appends.Load(), "an unchanged session writes nothing")
	require.EqualValues(t, 1, store.replaces.Load())

	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rows, 4)
	require.Equal(t, "haiku", record.Model)
	require.Equal(t, "HELLO", record.Title)
}

// BenchmarkCommitMirror measures one turn's commit of ten new rows on top of
// a large transcript.
func BenchmarkCommitMirror(b *testing.B) {
	for _, rows := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			dir := b.TempDir()
			id := "00000000-0000-4000-8000-000000000001"
			path := filepath.Join(dir, id+".jsonl")
			row := func(index int) string {
				return fmt.Sprintf(`{"type":"assistant","uuid":"%d","sessionId":"%s","message":{"id":"m%d","role":"assistant","content":[{"type":"text","text":"%s"}]}}`+"\n", index, id, index, strings.Repeat("x", 1000))
			}

			var history strings.Builder
			for index := range rows {
				history.WriteString(row(index))
			}

			require.NoError(b, os.WriteFile(path, []byte(history.String()), 0o600))

			store := acpcore.NewInMemorySessionStore()
			s := &session{agent: &Agent{store: store}, id: acp.SessionId(id), nativeID: id, cwd: dir, nativePath: path, mirror: sessionlog.NewMirror(store, id)}
			require.NoError(b, s.commitMirror(b.Context()))

			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(b, err)
			b.Cleanup(func() { _ = file.Close() })

			next := rows

			b.ReportAllocs()

			for b.Loop() {
				for range 10 {
					_, _ = file.WriteString(row(next))
					next++
				}

				if err := s.commitMirror(b.Context()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// logCapture records the adapter's structured log lines.
type logCapture struct {
	mu    sync.Mutex
	lines bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.lines.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.lines.String()
}

func (c *logCapture) option() Option {
	return WithLogger(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// appendFailureStore fails the next Append once armed, so the mirror
// recovers by republishing.
type appendFailureStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *appendFailureStore) Append(ctx context.Context, key acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.CompareAndSwap(true, false) {
		return errors.New("append unavailable")
	}

	return s.SessionStore.Append(ctx, key, base, entries, replacements)
}

// Every mirror recovery is logged once: a republish after a failed append,
// and a restore that repaired the native transcript.
func TestMirrorRecoveriesAreLogged(t *testing.T) {
	t.Parallel()
	logs := &logCapture{}
	store := &appendFailureStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store), logs.option())
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	store.fail.Store(true)
	_, err = h.prompt(created.SessionId, "HELLO", nil)
	require.NoError(t, err, "a failed append recovers by republishing")
	require.Equal(t, 1, strings.Count(logs.String(), "session mirror republished"))
	require.Contains(t, logs.String(), "reason=append_failed")

	s, err := h.agent.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	path := s.nativePath
	s.mu.Unlock()
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(`{"type":"user","mess`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(logs.String(), "native session file repaired on restore"))
	require.Contains(t, logs.String(), "torn_bytes_cut=20")
}
