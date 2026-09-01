package claudeacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func testStoredSessionEntries(t *testing.T, options ClaudeOptions, entries ...SessionStoreEntry) []SessionStoreEntry {
	t.Helper()

	configuration, err := marshalSessionConfiguration(configurationFromOptions(options))
	require.NoError(t, err)

	return append([]SessionStoreEntry{configuration}, entries...)
}

func requireSessionResumeIncompatible(t *testing.T, err error, field string) {
	t.Helper()

	var requestError *acp.RequestError
	require.ErrorAs(t, err, &requestError)
	require.Equal(t, -32602, requestError.Code)
	require.Equal(t, map[string]any{
		jsonFieldError: "session_resume_incompatible",
		jsonFieldField: field,
	}, requestError.Data)
}

func TestSessionConfigurationCodecIsExact(t *testing.T) {
	t.Parallel()

	configuration := sessionConfiguration{
		Env:           map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"},
		ExtraPathDirs: []string{"/opt/first", "/opt/second"},
	}
	entry, err := marshalSessionConfiguration(configuration)
	require.NoError(t, err)

	decoded, err := unmarshalSessionConfiguration(entry)
	require.NoError(t, err)
	require.Equal(t, configuration, decoded)

	invalid := []string{
		`{}`,
		`{"type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":[],"unknown":true}`,
		`{"type":"acp_session_configuration","type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1.0,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":2,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":null,"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":{"TOKEN":"a","TOKEN":"b"},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":{"PATH":"/tmp"},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":["relative"]}`,
		`{"type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":[]} true`,
	}
	for _, value := range invalid {
		_, err := unmarshalSessionConfiguration(json.RawMessage(value))
		require.Error(t, err, value)
	}
}

func TestResumeSessionConfigurationInheritsAndRejectsConflicts(t *testing.T) {
	t.Parallel()

	stored := sessionConfiguration{
		Env:           map[string]string{"TOKEN": "stored"},
		ExtraPathDirs: []string{"/stored/first", "/stored/second"},
	}

	inherited, err := resumeSessionConfiguration(ClaudeOptions{}, sessionConfigurationPresence{}, stored)
	require.NoError(t, err)
	require.Equal(t, stored.Env, inherited.Env)
	require.Equal(t, stored.ExtraPathDirs, inherited.ExtraPathDirs)

	matching, err := resumeSessionConfiguration(ClaudeOptions{
		Env:           map[string]string{"TOKEN": "stored"},
		ExtraPathDirs: []string{"/stored/first", "/stored/second"},
	}, sessionConfigurationPresence{env: true, extraPathDirs: true}, stored)
	require.NoError(t, err)
	require.Equal(t, inherited.Env, matching.Env)
	require.Equal(t, inherited.ExtraPathDirs, matching.ExtraPathDirs)

	_, err = resumeSessionConfiguration(
		ClaudeOptions{Env: map[string]string{"TOKEN": "changed"}},
		sessionConfigurationPresence{env: true},
		stored,
	)
	requireSessionResumeIncompatible(t, err, metaOptionPath(settingsFieldEnv))

	_, err = resumeSessionConfiguration(
		ClaudeOptions{ExtraPathDirs: []string{"/stored/second", "/stored/first"}},
		sessionConfigurationPresence{extraPathDirs: true},
		stored,
	)
	requireSessionResumeIncompatible(t, err, metaOptionPath(metaExtraPathDirsKey))
}

func TestSessionMirrorWritesConfigurationOnceAheadOfTranscript(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"
	home := t.TempDir()
	store := NewInMemorySessionStore()
	session := &agentSession{configuration: sessionConfiguration{
		Env:           map[string]string{"TOOL_TOKEN": "stored"},
		ExtraPathDirs: []string{"/opt/tools"},
	}}
	mirror := newSessionMirror(nil, store, home, session)
	path := filepath.Join(home, "projects", "workspace", sessionID+".jsonl")

	require.NoError(t, mirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{
		FilePath: path,
		Entries:  []json.RawMessage{json.RawMessage(`{"type":"user"}`)},
	}))
	require.NoError(t, mirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{
		FilePath: path,
		Entries:  []json.RawMessage{json.RawMessage(`{"type":"assistant"}`)},
	}))

	entries, err := store.Load(t.Context(), SessionKey{SessionID: sessionID})
	require.NoError(t, err)
	require.Len(t, entries, 3)
	configuration, err := unmarshalSessionConfiguration(entries[0])
	require.NoError(t, err)
	require.Equal(t, session.configuration, configuration)
	require.JSONEq(t, `{"type":"user"}`, string(entries[1]))
	require.JSONEq(t, `{"type":"assistant"}`, string(entries[2]))
}

func TestColdLoadReconstructsStoredEnvironmentAndOrderedPath(t *testing.T) {
	const sessionID = "22222222-2222-4222-8222-222222222222"
	cwd := t.TempDir()
	storedOptions := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "stored"},
		ExtraPathDirs: []string{"/opt/first", "/opt/second"},
	}
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: sessionID}, testStoredSessionEntries(
		t, storedOptions, []byte(`{"type":"user","message":{"content":"hello"}}`),
	)))

	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	_, err := agent.LoadSession(t.Context(), LoadSessionRequest(sessionID, cwd))
	require.NoError(t, err)
	session := agent.sessions[sessionID]
	require.Equal(t, "stored", session.clientOptions.Env["TOOL_TOKEN"])
	require.Equal(t, []string{"/opt/first", "/opt/second"}, session.clientOptions.ExtraPathDirs)

	materialized, err := os.ReadFile(session.materialized.mainPath)
	require.NoError(t, err)
	require.NotContains(t, string(materialized), sessionConfigurationEntryType)
	require.Contains(t, string(materialized), `"type":"user"`)
	require.NoError(t, agent.Close())

	conflicting, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	_, err = conflicting.LoadSession(t.Context(), LoadSessionRequest(sessionID, cwd, WithSessionMeta(
		ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "changed"}}.Meta(),
	)))
	requireSessionResumeIncompatible(t, err, metaOptionPath(settingsFieldEnv))
	require.Empty(t, conflicting.sessions)
	require.NoError(t, conflicting.Close())
}

func TestTranscriptOnlyStoreIsNotARecoverableSessionRecord(t *testing.T) {
	t.Parallel()

	const sessionID = "33333333-3333-4333-8333-333333333333"
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(context.Background(), SessionKey{SessionID: sessionID}, []SessionStoreEntry{
		[]byte(`{"type":"user"}`),
	}))

	agent := NewAgent(WithSessionStore(store))
	_, err := agent.storedSession(t.Context(), sessionID)
	requireSessionResumeIncompatible(t, err, acpFieldSessionID)
}

func TestActiveResumeInheritsConfigurationWhenFieldsAreOmitted(t *testing.T) {
	cwd := t.TempDir()
	options := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "active"},
		ExtraPathDirs: []string{"/opt/active"},
	}
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	created, err := agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(options.Meta())))
	require.NoError(t, err)
	original := agent.sessions[created.SessionId]

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.Same(t, original, agent.sessions[created.SessionId])
	require.NoError(t, agent.Close())
}
