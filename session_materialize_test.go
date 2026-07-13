package claudeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMaterializeStoreSessionUsesScratchDir(t *testing.T) {
	ctx := context.Background()
	sessionID := "88888888-8888-4888-8888-888888888888"
	cwd := t.TempDir()
	sourceHome := t.TempDir()

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID}, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"hello"}}`),
	}))

	scratch := filepath.Join(t.TempDir(), "nested", "scratch")
	agent := NewAgent(WithSessionStore(store), WithScratchDir(scratch))

	materialized, err := agent.materializeStoreSession(ctx, sessionID, cwd, sourceHome, nil)
	require.NoError(t, err)
	require.NotNil(t, materialized)
	t.Cleanup(func() { require.NoError(t, materialized.Close()) })

	require.Equal(t, scratch, filepath.Dir(materialized.configDir))
	require.True(t, strings.HasPrefix(filepath.Base(materialized.configDir), "acp-go-claude-resume-"))

	occupied := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.WriteFile(occupied, []byte("x"), 0o600))

	blocked := NewAgent(WithSessionStore(store), WithScratchDir(occupied))
	_, err = blocked.materializeStoreSession(ctx, sessionID, cwd, sourceHome, nil)
	require.ErrorContains(t, err, "create scratch parent dir")
}

func TestMaterializeStoreSessionBranches(t *testing.T) {
	ctx := context.Background()
	sessionID := "11111111-1111-4111-8111-111111111111"
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	sourceHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceHome, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600))

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID}, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"hello"}}`),
		[]byte(`   `),
	}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "subagents/worker"}, []SessionStoreEntry{
		[]byte(`{"type":"agent_metadata","name":"worker","color":"blue"}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`),
	}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "../unsafe"}, []SessionStoreEntry{
		[]byte(`{"type":"assistant"}`),
	}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "subagents/empty"}, nil))

	agent := NewAgent(WithHome(sourceHome), WithSessionStore(store))
	materialized, err := agent.materializeStoreSession(ctx, sessionID, cwd, sourceHome, nil)
	require.NoError(t, err)
	require.NotNil(t, materialized)
	t.Cleanup(func() { require.NoError(t, materialized.Close()) })

	mainData, err := os.ReadFile(materialized.mainPath)
	require.NoError(t, err)
	require.Equal(t, "{\"type\":\"user\",\"message\":{\"content\":\"hello\"}}\n", string(mainData))

	copiedConfig, err := os.ReadFile(filepath.Join(materialized.configDir, ".claude.json"))
	require.NoError(t, err)
	require.JSONEq(t, `{"theme":"dark"}`, string(copiedConfig))

	subagentBase := filepath.Join(materialized.configDir, "projects", projectKey, sessionID, "subagents", "worker")
	transcript, err := os.ReadFile(subagentBase + ".jsonl")
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`, string(transcript))
	metadata, err := os.ReadFile(subagentBase + ".meta.json")
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"worker","color":"blue"}`, string(metadata))
	require.NoFileExists(t, filepath.Join(materialized.configDir, "projects", projectKey, sessionID, "..", "unsafe.jsonl"))

	require.NoError(t, materialized.Close())
	require.NoDirExists(t, materialized.configDir)
	require.NoError(t, ((*materializedSession)(nil)).Close())
	require.NoError(t, (&materializedSession{}).Close())

	none, err := agent.materializeStoreSession(ctx, "", cwd, sourceHome, nil)
	require.NoError(t, err)
	require.Nil(t, none)
	none, err = agent.materializeStoreSession(ctx, "not-a-uuid", cwd, sourceHome, nil)
	require.NoError(t, err)
	require.Nil(t, none)

	nativePath := filepath.Join(sourceHome, "projects", projectKey, sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(nativePath), 0o755))
	require.NoError(t, os.WriteFile(nativePath, []byte(`{}`), 0o600))
	none, err = agent.materializeStoreSession(ctx, sessionID, cwd, sourceHome, nil)
	require.NoError(t, err)
	require.Nil(t, none)
}

func TestMaterializeStoreSessionErrors(t *testing.T) {
	ctx := context.Background()
	sessionID := "22222222-2222-4222-8222-222222222222"
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	agent := NewAgent(WithSessionStore(store))

	originalCopy := copyClaudeConfigFiles
	copyClaudeConfigFiles = func(string, string, map[string]string) error { return errors.New("copy failed") }
	t.Cleanup(func() { copyClaudeConfigFiles = originalCopy })
	materialized, err := agent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.ErrorContains(t, err, "copy failed")
	require.Nil(t, materialized)
	copyClaudeConfigFiles = originalCopy

	listErr := errors.New("list failed")
	agent = NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, listSubkeysErr: listErr}))
	materialized, err = agent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.ErrorContains(t, err, "list session store subkeys")
	require.Nil(t, materialized)

	loadErr := errors.New("load failed")
	agent = NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, loadErr: loadErr}))
	materialized, err = agent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.ErrorContains(t, err, "load session store key")
	require.Nil(t, materialized)

	_, err = agent.materializeStoreSession(ctx, sessionID, "relative", "", nil)
	require.ErrorContains(t, err, "cwd must be an absolute path")

	require.ErrorContains(t, writeJSONFile(filepath.Join(t.TempDir(), "bad.json"), map[string]any{"bad": func() {}}), "encode metadata")

	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	require.ErrorContains(t, writeStoreJSONL(filepath.Join(blocker, "child.jsonl"), []SessionStoreEntry{[]byte(`{}`)}), "create transcript dir")
	require.ErrorContains(t, writeJSONFile(filepath.Join(blocker, "child.json"), map[string]any{"ok": true}), "create metadata dir")
}

func TestNativeTranscriptAndConfigDirHelpers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	envHome := t.TempDir()
	require.Equal(t, filepath.Clean(envHome), sourceClaudeConfigDir("", map[string]string{"CLAUDE_CONFIG_DIR": envHome}))

	explicitHome := t.TempDir()
	require.Equal(t, filepath.Clean(explicitHome), sourceClaudeConfigDir(explicitHome, nil))

	processHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", processHome)
	require.Equal(t, filepath.Clean(processHome), sourceClaudeConfigDir("", nil))
	require.Equal(t, filepath.Clean(processHome), defaultClaudeConfigDir(""))

	overrideHome := t.TempDir()
	require.Equal(t, filepath.Clean(overrideHome), defaultClaudeConfigDir(overrideHome))

	dst := t.TempDir()
	require.NoError(t, copyClaudeConfigFilesImpl(dst, dst, nil))
	require.NoFileExists(t, filepath.Join(dst, ".claude.json"))
	require.NoError(t, copyClaudeConfigFilesImpl(dst, "", map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir()}))

	sessionID := "33333333-3333-4333-8333-333333333333"
	projectKey := "project"
	source := t.TempDir()
	exists, err := claudeNativeTranscriptExists(source, nil, "", sessionID)
	require.NoError(t, err)
	require.False(t, exists)
	exists, err = claudeNativeTranscriptExists(source, nil, projectKey, "")
	require.NoError(t, err)
	require.False(t, exists)
	exists, err = claudeNativeTranscriptExists(source, nil, projectKey, sessionID)
	require.NoError(t, err)
	require.False(t, exists)

	path := filepath.Join(source, "projects", projectKey, sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	exists, err = claudeNativeTranscriptExists(source, nil, projectKey, sessionID)
	require.NoError(t, err)
	require.True(t, exists)

	require.NoError(t, deleteNativeTranscriptImpl(context.Background(), source, "bad"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, deleteNativeTranscriptImpl(cancelled, source, sessionID), context.Canceled)

	subagentDir := filepath.Join(source, "projects", "other", sessionID)
	require.NoError(t, os.MkdirAll(subagentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subagentDir, "frame.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, deleteNativeTranscriptImpl(context.Background(), source, sessionID))
	require.NoFileExists(t, path)
	require.NoDirExists(t, subagentDir)
}

func TestMaterializeFaultInjectionSeams(t *testing.T) {
	ctx := context.Background()
	sessionID := "66666666-6666-4666-8666-666666666666"
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "subagents/worker"}, []SessionStoreEntry{[]byte(`{"type":"assistant"}`)}))
	agent := NewAgent(WithSessionStore(store), WithSessionStoreLoadTimeout(time.Millisecond))

	originalMkdirTemp := materializeMkdirTemp
	originalWriteFile := materializeWriteFile
	originalStat := materializeStat
	originalReadFile := materializeReadFile
	originalGlob := materializeGlob
	originalRemove := materializeRemove
	originalRemoveAll := materializeRemoveAll
	originalUserHome := materializeUserHomeDir
	t.Cleanup(func() {
		materializeMkdirTemp = originalMkdirTemp
		materializeWriteFile = originalWriteFile
		materializeStat = originalStat
		materializeReadFile = originalReadFile
		materializeGlob = originalGlob
		materializeRemove = originalRemove
		materializeRemoveAll = originalRemoveAll
		materializeUserHomeDir = originalUserHome
	})

	materializeMkdirTemp = func(string, string) (string, error) { return "", errors.New("temp failed") }
	_, err := agent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.ErrorContains(t, err, "temp failed")
	materializeMkdirTemp = originalMkdirTemp

	materializeWriteFile = func(string, []byte, os.FileMode) error { return errors.New("write failed") }
	require.ErrorContains(t, writeStoreJSONL(filepath.Join(t.TempDir(), "out.jsonl"), []SessionStoreEntry{[]byte(`{}`)}), "write failed")
	_, err = agent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.ErrorContains(t, err, "write failed")
	materializeWriteFile = originalWriteFile

	materializeStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat failed") }
	_, err = claudeNativeTranscriptExists(t.TempDir(), nil, "project", sessionID)
	require.ErrorContains(t, err, "stat native Claude transcript")
	_, err = agent.materializeStoreSession(ctx, sessionID, cwd, t.TempDir(), nil)
	require.ErrorContains(t, err, "stat native Claude transcript")
	materializeStat = originalStat

	emptyStoreAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	none, err := emptyStoreAgent.materializeStoreSession(ctx, sessionID, cwd, "", nil)
	require.NoError(t, err)
	require.Nil(t, none)

	materializeReadFile = func(string) ([]byte, error) { return []byte(`{"ok":true}`), nil }
	materializeWriteFile = func(string, []byte, os.FileMode) error { return errors.New("copy write failed") }
	require.ErrorContains(t, copyClaudeConfigFilesImpl(t.TempDir(), t.TempDir(), nil), "copy write failed")
	materializeReadFile = originalReadFile
	materializeWriteFile = originalWriteFile

	materializeUserHomeDir = func() (string, error) { return "", errors.New("home failed") }
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.Equal(t, "", sourceClaudeConfigDir("", nil))
	require.Equal(t, filepath.Clean(".claude"), defaultClaudeConfigDir(""))
	require.NoError(t, deleteNativeTranscriptImpl(ctx, "", sessionID))
	materializeUserHomeDir = originalUserHome

	subkeyErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, loadSubpathErr: errors.New("subkey failed")}))
	err = subkeyErrAgent.materializeStoreSubkeys(ctx, subkeyErrAgent.sessionStore(), t.TempDir(), SessionKey{SessionID: sessionID})
	require.ErrorContains(t, err, "subkey failed")
	emptySubkeyAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), listSubkeys: []string{"empty"}}))
	require.NoError(t, emptySubkeyAgent.materializeStoreSubkeys(ctx, emptySubkeyAgent.sessionStore(), t.TempDir(), SessionKey{SessionID: sessionID}))
	materializeWriteFile = func(string, []byte, os.FileMode) error { return errors.New("subkey write failed") }
	err = agent.materializeStoreSubkeys(ctx, store, t.TempDir(), SessionKey{SessionID: sessionID})
	require.ErrorContains(t, err, "subkey write failed")
	materializeWriteFile = originalWriteFile
	metadataOnlyStore := NewInMemorySessionStore()
	require.NoError(t, metadataOnlyStore.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "subagents/meta"}, []SessionStoreEntry{[]byte(`{"type":"agent_metadata","name":"meta"}`)}))
	metadataOnlyAgent := NewAgent(WithSessionStore(metadataOnlyStore))
	materializeWriteFile = func(string, []byte, os.FileMode) error { return errors.New("metadata write failed") }
	err = metadataOnlyAgent.materializeStoreSubkeys(ctx, metadataOnlyStore, t.TempDir(), SessionKey{SessionID: sessionID})
	require.ErrorContains(t, err, "metadata write failed")
	materializeWriteFile = originalWriteFile

	materializeGlob = func(string) ([]string, error) { return nil, errors.New("glob failed") }
	require.ErrorContains(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID), "glob failed")

	materializeGlob = func(string) ([]string, error) { return []string{"one"}, nil }
	materializeRemove = func(string) error { return errors.New("remove failed") }
	require.ErrorContains(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID), "remove failed")
	materializeRemove = func(string) error { return os.ErrNotExist }
	require.NoError(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID))

	call := 0
	materializeGlob = func(string) ([]string, error) {
		call++
		if call == 1 {
			return nil, nil
		}

		return []string{"dir"}, nil
	}
	materializeGlobErr := errors.New("dir glob failed")
	call = 0
	materializeGlob = func(string) ([]string, error) {
		call++
		if call == 1 {
			return nil, nil
		}

		return nil, materializeGlobErr
	}
	require.ErrorIs(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID), materializeGlobErr)
	call = 0
	materializeGlob = func(string) ([]string, error) {
		call++
		if call == 1 {
			return nil, nil
		}

		return []string{"dir"}, nil
	}
	materializeRemoveAll = func(string) error { return errors.New("remove all failed") }
	require.ErrorContains(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID), "remove all failed")
	materializeRemoveAll = func(string) error { return os.ErrNotExist }
	require.NoError(t, deleteNativeTranscriptImpl(ctx, t.TempDir(), sessionID))

	dirCancelCtx, dirCancel := context.WithCancel(ctx)
	call = 0
	materializeGlob = func(string) ([]string, error) {
		call++
		if call == 1 {
			return nil, nil
		}

		return []string{"one", "two"}, nil
	}
	materializeRemoveAll = func(string) error {
		dirCancel()

		return nil
	}
	require.ErrorIs(t, deleteNativeTranscriptImpl(dirCancelCtx, t.TempDir(), sessionID), context.Canceled)

	cancelCtx, cancel := context.WithCancel(ctx)
	materializeGlob = func(string) ([]string, error) { return []string{"one", "two"}, nil }
	materializeRemove = func(string) error {
		cancel()

		return nil
	}
	require.ErrorIs(t, deleteNativeTranscriptImpl(cancelCtx, t.TempDir(), sessionID), context.Canceled)
}

type faultSessionStore struct {
	SessionStore
	appendErr       error
	loadErr         error
	loadSubpathErr  error
	replaceErr      error
	deleteErr       error
	listSessions    []SessionSummary
	listSessionsErr error
	listSubkeys     []string
	listSubkeysErr  error
}

func (s *faultSessionStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if s.appendErr != nil {
		return s.appendErr
	}

	return s.SessionStore.Append(ctx, key, entries)
}

func (s *faultSessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if s.loadSubpathErr != nil && key.Subpath != SessionStoreMainSubpath {
		return nil, s.loadSubpathErr
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	return s.SessionStore.Load(ctx, key)
}

func (s *faultSessionStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if s.replaceErr != nil {
		return s.replaceErr
	}

	return s.SessionStore.Replace(ctx, main, replacements)
}

func (s *faultSessionStore) Delete(ctx context.Context, key SessionKey) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}

	return s.SessionStore.Delete(ctx, key)
}

func (s *faultSessionStore) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	if s.listSessionsErr != nil {
		return nil, s.listSessionsErr
	}
	if s.listSessions != nil {
		return append([]SessionSummary(nil), s.listSessions...), nil
	}

	return s.SessionStore.ListSessions(ctx)
}

func (s *faultSessionStore) ListSubkeys(ctx context.Context, key SessionKey) ([]string, error) {
	if s.listSubkeysErr != nil {
		return nil, s.listSubkeysErr
	}
	if s.listSubkeys != nil {
		return append([]string(nil), s.listSubkeys...), nil
	}

	return s.SessionStore.ListSubkeys(ctx, key)
}
