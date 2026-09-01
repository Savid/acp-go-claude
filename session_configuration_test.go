package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		``,
		`[]`,
		`{"`,
		`{"type":`,
		`{"type":"acp_session_configuration"`,
		`{}`,
		`{"type":1,"version":1,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":[],"unknown":true}`,
		`{"type":"acp_session_configuration","type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1.0,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":2,"env":{},"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":null,"extraPathDirs":[]}`,
		`{"type":"acp_session_configuration","version":1,"env":{},"extraPathDirs":null}`,
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

func TestSessionConfigurationEnvironmentDecoderIsExact(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		``,
		`[]`,
		`{"`,
		`{"TOKEN":`,
		`{"TOKEN":"value"`,
		`{"TOKEN":1}`,
		`{"TOKEN":"one","TOKEN":"two"}`,
		`{"TOKEN":"value"} true`,
	} {
		_, err := decodeSessionConfigurationEnv(json.RawMessage(value))
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

func TestActiveNonCarrierMismatchDoesNotRetirePredecessor(t *testing.T) {
	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport)
	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	original := agent.sessions[response.SessionId]

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(response.SessionId, t.TempDir()))
	requireSessionResumeIncompatible(t, err, acpFieldSessionID)
	require.Same(t, original, agent.sessions[response.SessionId])
	require.Zero(t, transport.CloseCalls())
	require.NoError(t, agent.Close())
}

func TestActiveCarrierChangeRetiresThenDurablyPublishesReplacement(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	originalOptions := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "old"},
		ExtraPathDirs: []string{"/opt/old-first", "/opt/old-second"},
	}
	requestedOptions := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "new"},
		ExtraPathDirs: []string{"/opt/new-second", "/opt/new-first"},
	}

	first := newFakeClaudeTransport()
	second := newFakeClaudeTransport()
	created := 0
	agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(store))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		created++
		if created == 1 {
			return claude.NewClient(log, options, first)
		}

		require.Equal(t, 1, first.CloseCalls(), "the predecessor is contained before successor construction")

		return claude.NewClient(log, options, second)
	}

	createdSession, err := agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(originalOptions.Meta())))
	require.NoError(t, err)
	original := agent.sessions[createdSession.SessionId]
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(createdSession.SessionId)},
		testStoredSessionEntries(t, originalOptions, []byte(`{"type":"user"}`))))

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(
		createdSession.SessionId, cwd, WithSessionMeta(requestedOptions.Meta()),
	))
	require.NoError(t, err)
	replacement := agent.sessions[createdSession.SessionId]
	require.NotSame(t, original, replacement)
	require.Equal(t, requestedOptions.Env, replacement.configuration.Env)
	require.Equal(t, requestedOptions.ExtraPathDirs, replacement.configuration.ExtraPathDirs)
	require.Equal(t, 1, first.CloseCalls())

	entries, err := store.Load(t.Context(), SessionKey{SessionID: string(createdSession.SessionId)})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	configuration, err := unmarshalSessionConfiguration(entries[0])
	require.NoError(t, err)
	require.Equal(t, configurationFromOptions(requestedOptions), configuration)
	require.JSONEq(t, `{"type":"user"}`, string(entries[1]))
	require.NoError(t, agent.Close())
}

func TestFailedActiveCarrierSuccessorLeavesColdCommittedPredecessor(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	originalOptions := ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "old"}}
	requestedOptions := ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "new"}}
	first := newFakeClaudeTransport()
	failed := newFakeClaudeTransport()
	failed.startErr = errors.New("successor start failed")
	retry := newFakeClaudeTransport()
	transports := []*fakeClaudeTransport{first, failed, retry}
	created := 0

	agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(store))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		transport := transports[created]
		created++

		return claude.NewClient(log, options, transport)
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(originalOptions.Meta())))
	require.NoError(t, err)
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(response.SessionId)},
		testStoredSessionEntries(t, originalOptions, []byte(`{"type":"user"}`))))

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(
		response.SessionId, cwd, WithSessionMeta(requestedOptions.Meta()),
	))
	require.ErrorContains(t, err, "successor start failed")
	require.NotContains(t, agent.sessions, response.SessionId)
	require.Equal(t, 1, first.CloseCalls())

	entries, err := store.Load(t.Context(), SessionKey{SessionID: string(response.SessionId)})
	require.NoError(t, err)
	configuration, err := unmarshalSessionConfiguration(entries[0])
	require.NoError(t, err)
	require.Equal(t, configurationFromOptions(originalOptions), configuration)

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(response.SessionId, cwd))
	require.NoError(t, err)
	require.Equal(t, originalOptions.Env, agent.sessions[response.SessionId].configuration.Env)
	require.NoError(t, agent.Close())
}

func TestReplacementConfigurationMustCommitBeforePublication(t *testing.T) {
	cwd := t.TempDir()
	backing := NewInMemorySessionStore()
	store := &faultSessionStore{SessionStore: backing}
	originalOptions := ClaudeOptions{ExtraPathDirs: []string{"/opt/old"}}
	requestedOptions := ClaudeOptions{ExtraPathDirs: []string{"/opt/new"}}
	first := newFakeClaudeTransport()
	second := newFakeClaudeTransport()
	created := 0

	agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(store))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		created++
		if created == 1 {
			return claude.NewClient(log, options, first)
		}

		return claude.NewClient(log, options, second)
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(originalOptions.Meta())))
	require.NoError(t, err)
	require.NoError(t, backing.Append(t.Context(), SessionKey{SessionID: string(response.SessionId)},
		testStoredSessionEntries(t, originalOptions, []byte(`{"type":"user"}`))))
	store.replaceErr = errors.New("configuration commit failed")

	_, err = agent.LoadSession(t.Context(), LoadSessionRequest(
		response.SessionId, cwd, WithSessionMeta(requestedOptions.Meta()),
	))
	require.ErrorContains(t, err, "configuration commit failed")
	require.NotContains(t, agent.sessions, response.SessionId)
	require.Equal(t, 1, second.CloseCalls(), "an uncommitted successor is contained")

	entries, err := backing.Load(t.Context(), SessionKey{SessionID: string(response.SessionId)})
	require.NoError(t, err)
	configuration, err := unmarshalSessionConfiguration(entries[0])
	require.NoError(t, err)
	require.Equal(t, configurationFromOptions(originalOptions), configuration)
	require.NoError(t, agent.Close())
}

func TestSessionLifecycleFlightsSerializeByIDAndCleanCanceledWaiters(t *testing.T) {
	agent := NewAgent()
	_, releaseA, err := agent.acquireSessionLifecycle(t.Context(), "a")
	require.NoError(t, err)

	_, releaseB, err := agent.acquireSessionLifecycle(t.Context(), "b")
	require.NoError(t, err, "an independent id progresses")
	releaseB()

	waitCtx, cancelWait := context.WithCancel(t.Context())
	waitDone := make(chan error, 1)
	go func() {
		_, _, err := agent.acquireSessionLifecycle(waitCtx, "a")
		waitDone <- err
	}()
	cancelWait()
	require.ErrorIs(t, <-waitDone, context.Canceled)

	entered := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_, release, err := agent.acquireSessionLifecycle(context.Background(), "a")
		if err != nil {
			return
		}
		close(entered)
		<-released
		release()
	}()

	select {
	case <-entered:
		t.Fatal("same-id lifecycle crossed its predecessor")
	case <-time.After(10 * time.Millisecond):
	}

	releaseA()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("same-id lifecycle did not enter after release")
	}
	close(released)

	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return len(agent.lifecycleFlights) == 0
	}, time.Second, time.Millisecond)
	require.NoError(t, agent.Close())
}

func TestAgentCloseWaitsForLifecycleFlightAndRefusesLaterAdmission(t *testing.T) {
	agent := NewAgent()
	lifecycleCtx, release, err := agent.acquireSessionLifecycle(t.Context(), "session")
	require.NoError(t, err)

	waiterDone := make(chan error, 1)
	go func() {
		_, _, acquireErr := agent.acquireSessionLifecycle(context.Background(), "session")
		waiterDone <- acquireErr
	}()
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.lifecycleFlights["session"].waiters == 2
	}, time.Second, time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- agent.Close() }()

	select {
	case <-lifecycleCtx.Done():
		requireAgentClosedRefusal(t, context.Cause(lifecycleCtx))
	case <-time.After(time.Second):
		t.Fatal("agent close did not cancel the lifecycle owner")
	}
	requireAgentClosedRefusal(t, <-waiterDone)

	select {
	case <-closed:
		t.Fatal("agent close crossed an active lifecycle flight")
	case <-time.After(10 * time.Millisecond):
	}

	release()
	release()
	require.NoError(t, <-closed)
	_, _, err = agent.acquireSessionLifecycle(t.Context(), "session")
	requireAgentClosedRefusal(t, err)
}
