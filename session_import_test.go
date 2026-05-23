package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

const testSessionID = "00000000-0000-4000-8000-000000000000"

func TestClaudeSessionImportExtensionCommitAndAbort(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	agent := NewAgent(WithClaudeHome(t.TempDir()))

	chunkResp, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-1",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"format":    claudeSessionImportFormat,
		"offset":    0,
		"entries": []any{
			map[string]any{"type": "user", "message": map[string]any{"content": "hello"}},
		},
	}))
	require.NoError(t, err)
	chunkMap, ok := chunkResp.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "import-1", chunkMap[jsonFieldImportID])

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-1",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"subpath":   "subagents/agent-a",
		"offset":    0,
		"entries": []any{
			map[string]any{"type": "agent_metadata", "description": "agent"},
			map[string]any{"type": "assistant", "message": map[string]any{"content": []any{}}},
		},
	}))
	require.NoError(t, err)

	commitResp, err := agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{
		"importId": "import-1",
	}))
	require.NoError(t, err)
	commitMap, ok := commitResp.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(3), asFloat(commitMap[jsonFieldEntries]))

	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	mainEntries, err := agent.importStore.Load(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: testSessionID})
	require.NoError(t, err)
	require.Len(t, mainEntries, 1)
	subEntries, err := agent.importStore.Load(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: testSessionID, Subpath: "subagents/agent-a"})
	require.NoError(t, err)
	require.Len(t, subEntries, 2)

	abortResp, err := agent.HandleExtensionMethod(context.Background(), claudeSessionAbortImportMethod, mustJSON(t, map[string]any{
		"importId": "missing",
	}))
	require.NoError(t, err)
	abortMap, ok := abortResp.(map[string]any)
	require.True(t, ok)
	aborted, ok := abortMap["aborted"].(bool)
	require.True(t, ok)
	require.False(t, aborted)

	_, err = agent.HandleExtensionMethod(context.Background(), "_claude/session/missing", nil)
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32601, reqErr.Code)
}

func TestClaudeSessionImportSingleRequestAndDispatcherResult(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)

	result, reqErr := conn.handle(context.Background(), claudeSessionImportMethod, mustJSON(t, map[string]any{
		"sessionId": testSessionID,
		"cwd":       t.TempDir(),
		"entries": []any{
			map[string]any{"type": "user", "message": map[string]any{"content": "hello"}},
		},
	}))
	require.Nil(t, reqErr)
	resp, ok := result.(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, resp[jsonFieldImportID])
	require.Equal(t, testSessionID, resp[acpFieldSessionID])
}

func TestClaudeSessionImportValidation(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	cwd := t.TempDir()

	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{name: "missing session", params: map[string]any{"cwd": cwd, "entries": []any{map[string]any{"type": "user"}}}},
		{name: "bad session", params: map[string]any{"sessionId": "bad", "cwd": cwd, "entries": []any{map[string]any{"type": "user"}}}},
		{name: "bad cwd", params: map[string]any{"sessionId": testSessionID, "cwd": "relative", "entries": []any{map[string]any{"type": "user"}}}},
		{name: "bad format", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "format": "other", "entries": []any{map[string]any{"type": "user"}}}},
		{name: "bad subpath", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "subpath": "../bad", "entries": []any{map[string]any{"type": "user"}}}},
		{name: "bad offset", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "offset": -1, "entries": []any{map[string]any{"type": "user"}}}},
		{name: "empty entries", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "entries": []any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, tc.params))
			requireInvalidParams(t, err)
		})
	}

	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, []byte(`{`))
	requireInvalidParams(t, err)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = agent.HandleExtensionMethod(cancelled, claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []json.RawMessage{json.RawMessage(`{"type":"user"}`)},
	}))
	require.ErrorIs(t, err, context.Canceled)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, json.RawMessage(`{"sessionId":"`+testSessionID+`","cwd":"`+cwd+`","entries":[1]}`))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, json.RawMessage(`{"sessionId":"`+testSessionID+`","cwd":"`+cwd+`","entries":[{}{}]}`))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-offset",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"offset":    1,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-mismatch",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"offset":    0,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-mismatch",
		"sessionId": "11111111-1111-4111-8111-111111111111",
		"cwd":       cwd,
		"offset":    1,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{"importId": ""}))
	requireInvalidParams(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{"importId": "missing"}))
	requireInvalidParams(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionAbortImportMethod, []byte(`{`))
	requireInvalidParams(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionAbortImportMethod, mustJSON(t, map[string]any{"importId": ""}))
	requireInvalidParams(t, err)
}

func TestClaudeSessionImportHashAndReplacement(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionStore()
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	cwd := t.TempDir()

	entry := json.RawMessage(`{"type":"user"}`)
	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-hash",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []json.RawMessage{entry},
	}))
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{
		"importId": "import-hash",
		"sha256":   "bad",
	}))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-hash-2",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []json.RawMessage{entry},
	}))
	require.NoError(t, err)

	hash := sha256.Sum256([]byte("{\"type\":\"user\"}\n"))
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{
		"importId": "import-hash-2",
		"sha256":   hex.EncodeToString(hash[:]),
	}))
	require.NoError(t, err)

	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	oldSubkey := SessionKey{ProjectKey: projectKey, SessionID: testSessionID, Subpath: "subagents/old"}
	require.NoError(t, store.Append(context.Background(), oldSubkey, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))

	replaceEntry := json.RawMessage(`{"type":"assistant"}`)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-replace",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []json.RawMessage{replaceEntry},
	}))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{
		"importId": "import-replace",
	}))
	require.NoError(t, err)

	loaded, err := store.Load(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: testSessionID})
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.JSONEq(t, string(replaceEntry), string(loaded[0]))

	subkeys, err := store.ListSubkeys(context.Background(), SessionKey{ProjectKey: projectKey, SessionID: testSessionID})
	require.NoError(t, err)
	require.Empty(t, subkeys)
}

func TestMaterializeStoreSessionAndResumeWiring(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	claudeHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(claudeHome, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"secret"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(claudeHome, ".claude.json"), []byte(`{"ok":true}`), 0o600))

	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{
		json.RawMessage(`{"type":"user","message":{"content":"hello"}}`),
	}))
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID, Subpath: "subagents/agent-a"}, []SessionStoreEntry{
		json.RawMessage(`{"type":"agent_metadata","description":"agent"}`),
		json.RawMessage(`{"type":"assistant","message":{"content":[]}}`),
	}))

	fake := newAgentFakeTransport()
	var captured claude.Options
	agent := NewAgent(
		WithClaudeHome(claudeHome),
		WithSessionStore(store),
		WithSessionStoreLoadTimeout(time.Second),
	)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(nil, options, fake)
	}

	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  testSessionID,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.NotEqual(t, claudeHome, captured.ClaudeHome)
	require.True(t, captured.SessionMirror)

	mainPath := filepath.Join(captured.ClaudeHome, "projects", projectKey, testSessionID+".jsonl")
	require.FileExists(t, mainPath)
	require.FileExists(t, filepath.Join(captured.ClaudeHome, "projects", projectKey, testSessionID, "subagents", "agent-a.jsonl"))
	metaPath := filepath.Join(captured.ClaudeHome, "projects", projectKey, testSessionID, "subagents", "agent-a.meta.json")
	require.FileExists(t, metaPath)
	credentials, err := os.ReadFile(filepath.Join(captured.ClaudeHome, ".credentials.json"))
	require.NoError(t, err)
	require.NotContains(t, string(credentials), "refreshToken")

	session, err := agent.session(testSessionID)
	require.NoError(t, err)
	require.NotNil(t, session.materialized)
	require.NoError(t, session.Close(context.Background()))
	require.NoFileExists(t, mainPath)
}

func TestMaterializeStoreSessionBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	materialized, err := agent.materializeStoreSession(ctx, "", cwd, "", nil)
	require.NoError(t, err)
	require.Nil(t, materialized)

	materialized, err = agent.materializeStoreSession(ctx, "not-a-uuid", cwd, "", nil)
	require.NoError(t, err)
	require.Nil(t, materialized)

	_, err = agent.materializeStoreSession(ctx, testSessionID, "relative", "", nil)
	require.Error(t, err)

	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(failingSessionStore{}))
	_, err = agent.materializeStoreSession(ctx, testSessionID, cwd, "", nil)
	require.Error(t, err)

	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(NewInMemorySessionStore()))
	materialized, err = agent.materializeStoreSession(ctx, testSessionID, cwd, "", nil)
	require.NoError(t, err)
	require.Nil(t, materialized)

	tooLongCwd := filepath.Join(string(filepath.Separator), strings.Repeat("a", 5000))
	tooLongProjectKey, err := projectKeyForDirectory(tooLongCwd)
	require.NoError(t, err)
	longStore := NewInMemorySessionStore()
	require.NoError(t, longStore.Append(ctx, SessionKey{ProjectKey: tooLongProjectKey, SessionID: testSessionID}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(longStore))
	_, err = agent.materializeStoreSession(ctx, testSessionID, tooLongCwd, "", nil)
	require.Error(t, err)

	storeWithBadSubkeys := NewInMemorySessionStore()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	require.NoError(t, storeWithBadSubkeys.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(subkeyListFailSessionStore{store: storeWithBadSubkeys}))
	_, err = agent.materializeStoreSession(ctx, testSessionID, cwd, "", nil)
	require.Error(t, err)

	func() {
		previousCopyClaudeAuthFiles := copyClaudeAuthFiles
		copyClaudeAuthFiles = func(string, string, map[string]string) error {
			return errors.New("copy failed")
		}
		defer func() { copyClaudeAuthFiles = previousCopyClaudeAuthFiles }()

		agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(storeWithBadSubkeys))
		_, err = agent.materializeStoreSession(ctx, testSessionID, cwd, "", nil)
		require.Error(t, err)
	}()

	tmpFile := filepath.Join(t.TempDir(), "tmp-file")
	require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0o600))
	t.Setenv("TMPDIR", tmpFile)

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	_, err = agent.materializeStoreSession(ctx, testSessionID, cwd, "", nil)
	require.Error(t, err)
}

func TestStartSessionMaterializedCleanupBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))

	bridgeAgent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithSessionStore(failingSessionStore{}),
		WithMCPProxyCommand("proxy"),
	)
	bridgeAgent.setConnection(&stubAgentClient{})
	_, err = bridgeAgent.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId: testSessionID,
		Cwd:       cwd,
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "ide", Id: "ide-1"}},
		},
	})
	require.Error(t, err)

	agent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithSessionStore(store),
		WithEnv(map[string]string{envClaudeModelConfig: `[`}),
	)
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: testSessionID, Cwd: cwd})
	require.Error(t, err)

	permissionHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(permissionHome, "acp-go-claude"), []byte("not a directory"), 0o600))
	agent = NewAgent(WithClaudeHome(permissionHome), WithSessionStore(store))
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: testSessionID, Cwd: cwd})
	require.Error(t, err)

	startErr := errors.New("start failed")
	fake := newAgentFakeTransport()
	fake.startErr = startErr
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: testSessionID, Cwd: cwd})
	require.ErrorIs(t, err, startErr)

	fake = newAgentFakeTransport()
	fake.initializeInfo = map[string]any{
		"models": []any{
			map[string]any{"value": "claude-opus-4-6", "displayName": "Opus"},
		},
	}
	fake.controlErrors = map[string]string{"set_model": "model failed"}
	agent = NewAgent(
		WithClaudeHome(t.TempDir()),
		WithSessionStore(store),
		WithEnv(map[string]string{envAnthropicModel: "opus"}),
	)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: testSessionID, Cwd: cwd})
	require.Error(t, err)
	require.True(t, fake.isClosed())
}

func TestMaterializeHelpers(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, (*materializedSession)(nil).Close())
	require.NoError(t, (&materializedSession{}).Close())

	fileParent := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(fileParent, []byte("x"), 0o600))
	require.Error(t, writeStoreJSONL(filepath.Join(fileParent, "nested.jsonl"), []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	require.Error(t, writeStoreJSONL(t.TempDir(), []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	emptyPath := filepath.Join(t.TempDir(), "empty.jsonl")
	require.NoError(t, writeStoreJSONL(emptyPath, []SessionStoreEntry{json.RawMessage(` `)}))
	emptyData, err := os.ReadFile(emptyPath)
	require.NoError(t, err)
	require.Empty(t, emptyData)

	require.Error(t, writeJSONFile(filepath.Join(fileParent, "meta.json"), map[string]any{"ok": true}))
	require.Error(t, writeJSONFile(filepath.Join(t.TempDir(), "meta.json"), make(chan struct{})))

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(`{"claudeAiOauth":{"refreshToken":"secret"}}`), 0o600))
	require.NoError(t, copyClaudeAuthFiles(t.TempDir(), source, map[string]string{"CLAUDE_CONFIG_DIR": source}))
	require.NoError(t, copyClaudeAuthFiles(source, source, nil))
	require.Error(t, copyClaudeAuthFiles(fileParent, source, nil))

	configOnly := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configOnly, ".claude.json"), []byte(`{"ok":true}`), 0o600))
	require.Error(t, copyClaudeAuthFiles(fileParent, configOnly, nil))

	require.Equal(t, []byte(`{`), redactClaudeRefreshToken([]byte(`{`)))
	require.JSONEq(t, `{"ok":true}`, string(redactClaudeRefreshToken([]byte(`{"ok":true}`))))
	require.Equal(t, source, sourceClaudeConfigDir("", map[string]string{"CLAUDE_CONFIG_DIR": source}))
	require.Equal(t, source, sourceClaudeConfigDir(source, nil))
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	require.Equal(t, source, sourceClaudeConfigDir("", nil))
	require.Equal(t, source, defaultClaudeConfigDir(""))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.Equal(t, filepath.Join(home, ".claude"), sourceClaudeConfigDir("", nil))
	require.Equal(t, filepath.Join(home, ".claude"), defaultClaudeConfigDir(""))
	t.Setenv("HOME", "")
	require.Empty(t, sourceClaudeConfigDir("", nil))
	require.Equal(t, filepath.Clean(".claude"), defaultClaudeConfigDir(""))

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	_, err = agent.loadStoreEntries(ctx, failingSessionStore{}, SessionKey{})
	require.Error(t, err)
}

func TestMaterializeStoreSubkeysBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	projectDir := t.TempDir()
	mainKey := SessionKey{ProjectKey: "project", SessionID: testSessionID}
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: "project", SessionID: testSessionID, Subpath: "../bad"}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: "project", SessionID: testSessionID, Subpath: "subagents/transcript"}, []SessionStoreEntry{json.RawMessage(`{"type":"assistant"}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: "project", SessionID: testSessionID, Subpath: "subagents/meta"}, []SessionStoreEntry{json.RawMessage(`{"type":"agent_metadata","name":"agent"}`)}))
	store.entries[SessionKey{ProjectKey: "project", SessionID: testSessionID, Subpath: "subagents/empty"}] = nil

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	require.NoError(t, agent.materializeStoreSubkeys(ctx, store, store, projectDir, mainKey))
	require.FileExists(t, filepath.Join(projectDir, testSessionID, "subagents", "transcript.jsonl"))
	require.FileExists(t, filepath.Join(projectDir, testSessionID, "subagents", "meta.meta.json"))

	require.Error(t, agent.materializeStoreSubkeys(ctx, store, errorSubkeyLister{}, projectDir, mainKey))
	require.Error(t, agent.materializeStoreSubkeys(ctx, subkeyLoadFailStore{}, fixedSubkeyLister{subkeys: []string{"subagents/fail"}}, projectDir, mainKey))

	projectFile := filepath.Join(t.TempDir(), "project-file")
	require.NoError(t, os.WriteFile(projectFile, []byte("x"), 0o600))
	require.Error(t, agent.materializeStoreSubkeys(ctx, store, fixedSubkeyLister{subkeys: []string{"subagents/transcript"}}, projectFile, mainKey))
	require.Error(t, agent.materializeStoreSubkeys(ctx, store, fixedSubkeyLister{subkeys: []string{"subagents/meta"}}, projectFile, mainKey))
}

func TestLoadAndListStoreSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{
		json.RawMessage(`{"type":"user","message":{"content":"stored title"}}`),
	}))

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}
	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	cwdPtr := cwd
	list, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, "stored title", *list.Sessions[0].Title)

	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  testSessionID,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) > 0
	}, time.Second, 10*time.Millisecond)
}

func TestStoreSessionListBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cwd := t.TempDir()
	cwdPtr := cwd

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(appendFailSessionStore{}))
	sessions, err := agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.NoError(t, err)
	require.Empty(t, sessions)
	sessions, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, sessions)
	sessions, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr, AdditionalDirectories: []string{t.TempDir()}})
	require.NoError(t, err)
	require.Empty(t, sessions)

	relative := "relative"
	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(listFailStore{}))
	_, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &relative})
	require.Error(t, err)

	_, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.Error(t, err)

	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(summaryLoadFailStore{}))
	_, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.Error(t, err)

	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(summaryStore{
		summaries: []SessionSummary{
			{SessionID: "bad"},
			{SessionID: testSessionID, MTime: 123},
		},
		entries: []SessionStoreEntry{json.RawMessage(`{"customTitle":"custom"}`)},
	}))
	sessions, err = agent.listStoreSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "custom", *sessions[0].Title)

	agent = NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(summaryStore{
		summaries: []SessionSummary{{SessionID: testSessionID, MTime: 123}},
		entries:   []SessionStoreEntry{json.RawMessage(`{"type":"user","message":{"content":"stored"}}`)},
	}))
	agent.sessions[testSessionID] = &Session{
		agent: agent,
		id:    testSessionID,
		turn:  make(chan struct{}, 1),
		cwd:   cwd,
	}
	resp, err := agent.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwdPtr})
	require.NoError(t, err)
	require.Len(t, resp.Sessions, 1)

	require.False(t, agent.storeHasSession(ctx, testSessionID, "relative"))
}

func TestLoadSessionStoreBackedMissingReplayPathClosesStartedSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	sessionID := acp.SessionId("not-a-uuid")

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{ProjectKey: projectKey, SessionID: string(sessionID)}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}))

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(store))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	require.True(t, fake.isClosed())

	agent.sessions[sessionID] = &Session{
		agent:       agent,
		id:          sessionID,
		turn:        make(chan struct{}, 1),
		cwd:         cwd,
		fingerprint: sessionStartFingerprint(sessionStart{Cwd: cwd, ResumeID: string(sessionID), McpServers: []acp.McpServer{}}),
	}
	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
}

func TestListSessionsStoreError(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(listFailStore{}))
	_, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	require.Error(t, err)
}

func TestStoreSessionTitleBranches(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ai", storeSessionTitle(testSessionID, []SessionStoreEntry{
		json.RawMessage(`{`),
		json.RawMessage(`{"aiTitle":" ai "}`),
	}))
	require.Equal(t, "custom", storeSessionTitle(testSessionID, []SessionStoreEntry{
		json.RawMessage(`{"customTitle":" custom "}`),
	}))
	require.Equal(t, "plain prompt", storeSessionTitle(testSessionID, []SessionStoreEntry{
		json.RawMessage(`{"type":"user","message":{"content":" plain   prompt "}}`),
	}))
	require.Equal(t, "block prompt", storeSessionTitle(testSessionID, []SessionStoreEntry{
		json.RawMessage(`{"type":"assistant","message":{"content":"ignored"}}`),
		json.RawMessage(`{"type":"user","message":{"content":[{"type":"tool_result"},{"type":"text","text":" block prompt "}]}}`),
	}))
	require.Equal(t, testSessionID, storeSessionTitle(testSessionID, []SessionStoreEntry{
		json.RawMessage(`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`),
	}))
	require.Empty(t, firstStoreUserPrompt(map[string]any{"type": "assistant"}))
}

func TestSessionMirrorPathAndPromptDrain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewInMemorySessionStore()
	claudeHome := t.TempDir()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(claudeHome), WithSessionStore(store))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	fake.mu.Lock()
	fake.systemMessages = []map[string]any{
		{
			"type":     "transcript_mirror",
			"filePath": filepath.Join(claudeHome, "projects", projectKey, string(resp.SessionId)+".jsonl"),
			"entries":  []any{map[string]any{"type": "user", "message": map[string]any{"content": "hello"}}},
		},
	}
	fake.mu.Unlock()

	_, err = agent.Prompt(ctx, acp.PromptRequest{SessionId: resp.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
	require.NoError(t, err)

	entries, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: string(resp.SessionId)})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	key, err := sessionKeyForMirrorPath(filepath.Join(claudeHome, "projects", projectKey, testSessionID, "subagents", "agent-a.meta.json"), filepath.Join(claudeHome, "projects"))
	require.NoError(t, err)
	require.Equal(t, "subagents/agent-a", key.Subpath)

	_, err = sessionKeyForMirrorPath(filepath.Join(t.TempDir(), "outside.jsonl"), filepath.Join(claudeHome, "projects"))
	require.Error(t, err)

	handled, err := (&Session{}).handleSessionMirror(ctx, &claude.TranscriptMirrorMessage{})
	require.NoError(t, err)
	require.True(t, handled)
}

func TestSessionMirrorBranches(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	claudeHome := t.TempDir()
	projectsDir := filepath.Join(claudeHome, "projects")
	projectKey := "-repo"

	require.Nil(t, newSessionMirror(nil, nil, claudeHome))

	mirror := newSessionMirror(nil, store, claudeHome)
	handled, err := mirror.handle(ctx, &claude.UserMessage{})
	require.NoError(t, err)
	require.False(t, handled)

	handled, err = mirror.handle(ctx, &claude.TranscriptMirrorMessage{})
	require.NoError(t, err)
	require.True(t, handled)

	handled, err = mirror.handle(ctx, &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(t.TempDir(), "outside.jsonl"),
		Entries:  []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)},
	})
	require.NoError(t, err)
	require.True(t, handled)

	failingMirror := newSessionMirror(nil, appendFailSessionStore{}, claudeHome)
	handled, err = failingMirror.handle(ctx, &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(projectsDir, projectKey, testSessionID+".jsonl"),
		Entries:  []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)},
	})
	require.Error(t, err)
	require.True(t, handled)

	retryStore := &retryAppendStore{failures: 2}
	err = appendMirrorEntries(ctx, retryStore, SessionKey{ProjectKey: projectKey, SessionID: testSessionID}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)})
	require.NoError(t, err)
	require.Equal(t, 3, retryStore.calls)

	err = appendMirrorEntries(ctx, appendFailSessionStore{}, SessionKey{}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)})
	require.Error(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = appendMirrorEntries(canceled, contextAwareAppendStore{}, SessionKey{}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)})
	require.ErrorIs(t, err, context.Canceled)

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	err = appendMirrorEntries(expired, contextAwareAppendStore{}, SessionKey{}, []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	for _, filePath := range []string{
		"",
		"relative.jsonl",
		filepath.Join(projectsDir, projectKey),
		filepath.Join(projectsDir, projectKey, "bad.jsonl"),
		filepath.Join(projectsDir, projectKey, testSessionID, "other", "agent.jsonl"),
		filepath.Join(projectsDir, projectKey, "bad-session", "subagents", "agent.jsonl"),
		filepath.Join(projectsDir, projectKey, testSessionID, "subagents", "agent.txt"),
		filepath.Join(projectsDir, projectKey, testSessionID, "subagents", "agent:bad.jsonl"),
	} {
		_, err = sessionKeyForMirrorPath(filePath, projectsDir)
		require.Error(t, err)
	}

	_, err = sessionKeyForMirrorPath(filepath.Join(projectsDir, projectKey, testSessionID+".jsonl"), "relative-projects")
	require.Error(t, err)

	key, err := sessionKeyForMirrorPath(filepath.Join(projectsDir, projectKey, testSessionID+".jsonl"), projectsDir)
	require.NoError(t, err)
	require.Empty(t, key.Subpath)
}

func TestSessionDrainMirrorBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.NoError(t, (&Session{}).drainSessionMirror(ctx))

	claudeHome := t.TempDir()
	projectsDir := filepath.Join(claudeHome, "projects")
	projectKey := "-repo"

	fake := newAgentFakeTransport()
	client := claude.NewClient(nil, claude.Options{}, fake)
	require.NoError(t, client.Start(ctx))
	defer func(c *claude.Client) { _ = c.Close() }(client)
	fake.incoming <- map[string]any{
		"type":     "transcript_mirror",
		"filePath": filepath.Join(projectsDir, projectKey, testSessionID+".jsonl"),
		"entries":  []any{map[string]any{"type": "user"}},
	}
	session := &Session{
		agent:  NewAgent(WithClaudeHome(claudeHome), WithSessionStore(appendFailSessionStore{})),
		id:     testSessionID,
		client: client,
		mirror: newSessionMirror(nil, appendFailSessionStore{}, claudeHome),
	}
	require.Error(t, session.drainSessionMirror(ctx))

	fake = newAgentFakeTransport()
	client = claude.NewClient(nil, claude.Options{}, fake)
	require.NoError(t, client.Start(ctx))
	defer func(c *claude.Client) { _ = c.Close() }(client)
	agent := NewAgent(WithClaudeHome(claudeHome), WithSessionStore(NewInMemorySessionStore()))
	agent.conn = &stubAgentClient{extensionErr: errors.New("extension failed")}
	fake.incoming <- map[string]any{
		"type":     "transcript_mirror",
		"filePath": filepath.Join(projectsDir, projectKey, testSessionID+".jsonl"),
		"entries":  []any{map[string]any{"type": "user"}},
	}
	session = &Session{
		agent:       agent,
		id:          testSessionID,
		client:      client,
		mirror:      newSessionMirror(nil, NewInMemorySessionStore(), claudeHome),
		rawMessages: rawMessageConfig{All: true},
	}
	require.Error(t, session.drainSessionMirror(ctx))

	fake = newAgentFakeTransport()
	client = claude.NewClient(nil, claude.Options{}, fake)
	require.NoError(t, client.Start(ctx))
	defer func(c *claude.Client) { _ = c.Close() }(client)
	fake.errs <- errors.New("receive failed")
	session = &Session{
		agent:  NewAgent(WithClaudeHome(claudeHome), WithSessionStore(NewInMemorySessionStore())),
		id:     testSessionID,
		client: client,
		mirror: newSessionMirror(nil, NewInMemorySessionStore(), claudeHome),
	}
	require.Error(t, session.drainSessionMirror(ctx))
}

func TestPromptSessionMirrorErrorBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	claudeHome := t.TempDir()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		setup func(fake *agentFakeTransport, sessionID acp.SessionId)
	}{
		{
			name: "live mirror",
			setup: func(fake *agentFakeTransport, sessionID acp.SessionId) {
				fake.mu.Lock()
				fake.systemMessages = []map[string]any{mirrorFrame(claudeHome, projectKey, string(sessionID))}
				fake.mu.Unlock()
			},
		},
		{
			name: "result drain",
			setup: func(fake *agentFakeTransport, sessionID acp.SessionId) {
				fake.setSuppressResult(true)
				fake.setSendHook(func(payload any) {
					raw, ok := payload.(map[string]any)
					if !ok || raw["type"] != "user" {
						return
					}

					fake.incoming <- successResultMessage()
					fake.incoming <- mirrorFrame(claudeHome, projectKey, string(sessionID))
				})
			},
		},
		{
			name: "system idle drain",
			setup: func(fake *agentFakeTransport, sessionID acp.SessionId) {
				fake.setSuppressResult(true)
				fake.setSendHook(func(payload any) {
					raw, ok := payload.(map[string]any)
					if !ok || raw["type"] != "user" {
						return
					}

					fake.incoming <- map[string]any{
						"type":    "system",
						"subtype": systemSubtypeSessionStateChanged,
						"data":    map[string]any{systemState: systemStateIdle},
					}
					fake.incoming <- mirrorFrame(claudeHome, projectKey, string(sessionID))
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newAgentFakeTransport()
			agent := NewAgent(WithClaudeHome(claudeHome), WithSessionStore(appendFailSessionStore{}))
			agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
				return claude.NewClient(nil, options, fake)
			}

			resp, err := agent.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd})
			require.NoError(t, err)
			tc.setup(fake, resp.SessionId)

			_, err = agent.Prompt(ctx, acp.PromptRequest{
				SessionId: resp.SessionId,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
			})
			require.Error(t, err)
		})
	}
}

func TestSessionImportStoreErrorBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(failingSessionStore{}))
	cwd := t.TempDir()
	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "import-fail",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{"importId": "import-fail"}))
	require.Error(t, err)

	require.False(t, validUUIDShape("bad"))
	require.False(t, validUUIDShape("00000000-0000-4000-8000-00000000000x"))
}

type failingSessionStore struct{}

func (failingSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return errors.New("append failed")
}

func (failingSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, errors.New("load failed")
}

func TestClaudeSessionImportEdgeValidation(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	cwd := t.TempDir()

	tooMany := make([]json.RawMessage, maxSessionImportChunkEntries+1)
	for i := range tooMany {
		tooMany[i] = json.RawMessage(`{"type":"user"}`)
	}

	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{name: "chunk entry limit", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "entries": tooMany}},
		{name: "line byte limit", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "entries": []json.RawMessage{json.RawMessage(`{"x":"` + strings.Repeat("x", maxSessionImportLineBytes) + `"}`)}}},
		{name: "null object", params: map[string]any{"sessionId": testSessionID, "cwd": cwd, "entries": []json.RawMessage{json.RawMessage(`null`)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, tc.params))
			requireInvalidParams(t, err)
		})
	}

	_, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(` `)})
	requireInvalidParams(t, err)
	_, _, err = validateSessionImportEntries([]json.RawMessage{json.RawMessage(`{} {}`)})
	requireInvalidParams(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportMethod, []byte(`{`))
	requireInvalidParams(t, err)
}

func TestClaudeSessionImportChunkStateValidation(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	cwd := t.TempDir()
	otherCwd := t.TempDir()

	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "state",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	require.NoError(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "state",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"offset":    0,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	requireInvalidParams(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "state",
		"sessionId": testSessionID,
		"cwd":       otherCwd,
		"offset":    1,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	requireInvalidParams(t, err)
}

func TestClaudeSessionImportAggregateLimitsAndUUIDError(t *testing.T) {
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	key := SessionKey{ProjectKey: projectKey, SessionID: testSessionID}

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.imports["count-limit"] = &sessionImport{
		ImportID:   "count-limit",
		SessionID:  testSessionID,
		Cwd:        cwd,
		ProjectKey: projectKey,
		entries:    map[SessionKey][]SessionStoreEntry{key: nil},
		count:      maxSessionImportEntries,
	}

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "count-limit",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	requireInvalidParams(t, err)

	agent.imports["byte-limit"] = &sessionImport{
		ImportID:   "byte-limit",
		SessionID:  testSessionID,
		Cwd:        cwd,
		ProjectKey: projectKey,
		entries:    map[SessionKey][]SessionStoreEntry{key: nil},
		bytes:      maxSessionImportBytes - 1,
	}

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "byte-limit",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []json.RawMessage{json.RawMessage(`{}`)},
	}))
	requireInvalidParams(t, err)

	random := uuidRandom
	uuidRandom = errReader{err: errors.New("random failed")}
	t.Cleanup(func() {
		uuidRandom = random
	})

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	require.Error(t, err)
}

func TestClaudeSessionImportReapsStaleImports(t *testing.T) {
	now := time.Unix(1000, 0)
	previousNow := sessionImportNow
	sessionImportNow = func() time.Time { return now }
	t.Cleanup(func() { sessionImportNow = previousNow })

	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.imports["stale"] = &sessionImport{
		ImportID:   "stale",
		SessionID:  testSessionID,
		Cwd:        cwd,
		ProjectKey: projectKey,
		entries:    map[SessionKey][]SessionStoreEntry{},
		UpdatedAt:  now.Add(-sessionImportTTL - time.Second),
	}

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
		"importId":  "fresh",
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   []any{map[string]any{"type": "user"}},
	}))
	require.NoError(t, err)

	agent.mu.Lock()
	_, staleExists := agent.imports["stale"]
	fresh := agent.imports["fresh"]
	agent.mu.Unlock()
	require.False(t, staleExists)
	require.NotNil(t, fresh)
	require.Equal(t, now, fresh.UpdatedAt)

	now = now.Add(sessionImportTTL + time.Second)
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{
		"importId": "fresh",
	}))
	requireInvalidParams(t, err)
}

func TestClaudeSessionImportCommitAndReplacementErrors(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	entry := []any{map[string]any{"type": "user"}}

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(appendFailSessionStore{}))
	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportMethod, mustJSON(t, map[string]any{
		"sessionId": testSessionID,
		"cwd":       cwd,
		"entries":   entry,
	}))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, []byte(`{`))
	requireInvalidParams(t, err)

	for _, tc := range []struct {
		name  string
		store SessionStore
	}{
		{name: "existing without replacer", store: existingNoReplaceSessionStore{}},
		{name: "replace failed", store: replaceFailSessionStore{}},
		{name: "append failed", store: appendFailSessionStore{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(WithClaudeHome(t.TempDir()), WithSessionStore(tc.store))
			_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionImportChunkMethod, mustJSON(t, map[string]any{
				"importId":  tc.name,
				"sessionId": testSessionID,
				"cwd":       cwd,
				"entries":   entry,
			}))
			require.NoError(t, err)

			_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionCommitImportMethod, mustJSON(t, map[string]any{"importId": tc.name}))
			require.Error(t, err)
		})
	}

	require.False(t, validUUIDShape("00000000x0000-4000-8000-000000000000"))
}

type existingNoReplaceSessionStore struct{}

func (existingNoReplaceSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (existingNoReplaceSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}, nil
}

type replaceFailSessionStore struct{}

func (replaceFailSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (replaceFailSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return []SessionStoreEntry{json.RawMessage(`{"type":"user"}`)}, nil
}

func (replaceFailSessionStore) ReplaceSession(context.Context, SessionKey, []SessionStoreReplacement) error {
	return errors.New("replace failed")
}

type appendFailSessionStore struct{}

func (appendFailSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return errors.New("append failed")
}

func (appendFailSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

type errorSubkeyLister struct{}

func (errorSubkeyLister) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, errors.New("list failed")
}

type subkeyListFailSessionStore struct {
	store *InMemorySessionStore
}

func (s subkeyListFailSessionStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	return s.store.Append(ctx, key, entries)
}

func (s subkeyListFailSessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	return s.store.Load(ctx, key)
}

func (subkeyListFailSessionStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, errors.New("list failed")
}

type fixedSubkeyLister struct {
	subkeys []string
}

func (l fixedSubkeyLister) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return l.subkeys, nil
}

type subkeyLoadFailStore struct{}

func (subkeyLoadFailStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (subkeyLoadFailStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, errors.New("load subkey failed")
}

type retryAppendStore struct {
	failures int
	calls    int
}

func (s *retryAppendStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	s.calls++
	if s.calls <= s.failures {
		return errors.New("temporary append failure")
	}

	return nil
}

func (s *retryAppendStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

type contextAwareAppendStore struct{}

func (contextAwareAppendStore) Append(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return errors.New("append failed")
}

func (contextAwareAppendStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

type listFailStore struct{}

func (listFailStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (listFailStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func (listFailStore) ListSessions(context.Context, string) ([]SessionSummary, error) {
	return nil, errors.New("list failed")
}

type summaryLoadFailStore struct{}

func (summaryLoadFailStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (summaryLoadFailStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, errors.New("load failed")
}

func (summaryLoadFailStore) ListSessions(context.Context, string) ([]SessionSummary, error) {
	return []SessionSummary{
		{SessionID: "bad"},
		{SessionID: testSessionID, MTime: 123},
	}, nil
}

type summaryStore struct {
	summaries []SessionSummary
	entries   []SessionStoreEntry
}

func (s summaryStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return nil
}

func (s summaryStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return s.entries, nil
}

func (s summaryStore) ListSessions(context.Context, string) ([]SessionSummary, error) {
	return s.summaries, nil
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)

	return data
}

func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	default:
		return 0
	}
}

func mirrorFrame(claudeHome string, projectKey string, sessionID string) map[string]any {
	return map[string]any{
		"type":     "transcript_mirror",
		"filePath": filepath.Join(claudeHome, "projects", projectKey, sessionID+".jsonl"),
		"entries":  []any{map[string]any{"type": "user"}},
	}
}
