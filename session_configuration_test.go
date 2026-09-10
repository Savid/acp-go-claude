package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func testStoredSessionEntries(t *testing.T, options ClaudeOptions, entries ...SessionStoreEntry) []SessionStoreEntry {
	t.Helper()

	configuration := marshalSessionConfiguration(configurationFromOptions(options))

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
		ExtraPathDirs: []string{absTestPath("opt", "first"), absTestPath("opt", "second")},
	}
	entry := marshalSessionConfiguration(configuration)

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

func TestActiveLoadRejectsStoredConfigurationDrift(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"env", "extraPathDirs"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			cwd := t.TempDir()
			store := NewInMemorySessionStore()
			transport := newFakeClaudeTransport()
			agent, _, _ := newFakeLifecycleAgent(t, transport, WithSessionStore(store))
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			created, err := agent.NewSession(t.Context(), NewSessionRequest(cwd))
			require.NoError(t, err)
			original := agent.sessions[created.SessionId]
			storedOptions := ClaudeOptions{}
			if field == "env" {
				storedOptions.Env = map[string]string{"TOOL_TOKEN": "divergent"}
			} else {
				storedOptions.ExtraPathDirs = []string{absTestPath("tools", "divergent")}
			}
			key := SessionKey{SessionID: string(created.SessionId)}
			require.NoError(t, store.Replace(t.Context(), key, []SessionStoreReplacement{{
				Key: key, Entries: testStoredSessionEntries(t, storedOptions, []byte(`{"type":"user"}`)),
			}}))
			_, err = agent.LoadSession(t.Context(), LoadSessionRequest(created.SessionId, cwd))
			requireSessionResumeIncompatible(t, err, metaOptionPath(field))
			require.Same(t, original, agent.sessions[created.SessionId])
			require.Zero(t, transport.CloseCalls())
		})
	}
}

func TestSessionMirrorWritesConfigurationOnceAheadOfTranscript(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"
	home := t.TempDir()
	store := NewInMemorySessionStore()
	session := &agentSession{id: sessionID, configuration: sessionConfiguration{
		Env:           map[string]string{"TOOL_TOKEN": "stored"},
		ExtraPathDirs: []string{absTestPath("opt", "tools")},
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
		ExtraPathDirs: []string{absTestPath("opt", "first"), absTestPath("opt", "second")},
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
	require.Equal(t, []string{absTestPath("opt", "first"), absTestPath("opt", "second")}, session.clientOptions.ExtraPathDirs)

	materialized, err := os.ReadFile(session.materialized.mainPath)
	require.NoError(t, err)
	require.NotContains(t, string(materialized), sessionConfigurationEntryType)
	require.Contains(t, string(materialized), `"type":"user"`)
	require.NoError(t, agent.Close())
}

func TestTranscriptOnlyStoreIsNotARecoverableSessionRecord(t *testing.T) {
	t.Parallel()

	const sessionID = "33333333-3333-4333-8333-333333333333"
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(context.Background(), SessionKey{SessionID: sessionID}, []SessionStoreEntry{
		[]byte(`{"type":"user"}`),
	}))

	// A store holding rows this adapter cannot read a session configuration out
	// of is an entry it found and cannot restore, not a caller-owned mismatch.
	agent := NewAgent(WithSessionStore(store))
	_, err := agent.storedSession(t.Context(), sessionID)
	requireClosedInternalFailure(t, err, restoreFailedError)
}

func TestActiveResumeInheritsConfigurationWhenFieldsAreOmitted(t *testing.T) {
	cwd := t.TempDir()
	options := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "active"},
		ExtraPathDirs: []string{absTestPath("opt", "active")},
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
		ExtraPathDirs: []string{absTestPath("opt", "old-first"), absTestPath("opt", "old-second")},
	}
	requestedOptions := ClaudeOptions{
		Env:           map[string]string{"TOOL_TOKEN": "new"},
		ExtraPathDirs: []string{absTestPath("opt", "new-second"), absTestPath("opt", "new-first")},
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
		[]SessionStoreEntry{[]byte(`{"type":"user"}`)}))

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
	originalOptions := ClaudeOptions{ExtraPathDirs: []string{absTestPath("opt", "old")}}
	requestedOptions := ClaudeOptions{ExtraPathDirs: []string{absTestPath("opt", "new")}}
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

func TestColdSessionConfigurationReplacement(t *testing.T) {
	t.Parallel()

	oldPaths := []string{absTestPath("tools", "first"), absTestPath("tools", "second")}
	newPaths := []string{absTestPath("tools", "new")}
	for _, operation := range []string{"load", "resume"} {
		for _, tc := range []struct {
			name    string
			options map[string]any
			token   string
			paths   []string
			writes  int32
		}{
			{name: "env", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}}, token: "rotated", paths: oldPaths, writes: 1},
			{name: "paths", options: map[string]any{"extraPathDirs": newPaths}, token: "stored", paths: newPaths, writes: 1},
			{name: "reordered paths", options: map[string]any{"extraPathDirs": []string{oldPaths[1], oldPaths[0]}}, token: "stored", paths: []string{oldPaths[1], oldPaths[0]}, writes: 1},
			{name: "both", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}, "extraPathDirs": newPaths}, token: "rotated", paths: newPaths, writes: 1},
			{name: "clear env", options: map[string]any{"env": map[string]string{}}, paths: oldPaths, writes: 1},
			{name: "clear paths", options: map[string]any{"extraPathDirs": []string{}}, token: "stored", writes: 1},
			{name: "omitted", token: "stored", paths: oldPaths},
			{name: "matching", options: map[string]any{"env": map[string]string{"TOOL_TOKEN": "stored"}, "extraPathDirs": oldPaths}, token: "stored", paths: oldPaths},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				const sessionID acp.SessionId = "33333333-3333-4333-8333-333333333333"
				cwd := t.TempDir()
				store := &countingConfigurationStore{SessionStore: NewInMemorySessionStore()}
				main := SessionKey{SessionID: string(sessionID)}
				sub := SessionKey{SessionID: string(sessionID), Subpath: "subagents/worker"}
				transcript := SessionStoreEntry(`{"type":"user","message":{"content":"remember the original conversation"}}`)
				subEntries := []SessionStoreEntry{[]byte(`{"type":"assistant","message":{"content":"worker history"}}`)}
				require.NoError(t, store.Append(t.Context(), main, testStoredSessionEntries(t,
					ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "stored"}, ExtraPathDirs: oldPaths}, transcript)))
				require.NoError(t, store.Append(t.Context(), sub, subEntries))

				wantedEnv := map[string]string{}
				if tc.token != "" {
					wantedEnv["TOOL_TOKEN"] = tc.token
				}
				for _, options := range []map[string]any{
					tc.options,
					{"env": wantedEnv, "extraPathDirs": slices.Clone(tc.paths)},
					nil,
				} {
					agent, launches := newColdConfigurationNativeAgent(t, store)
					err := coldConfigurationOperation(t.Context(), agent, operation, sessionID, cwd, options)
					require.NoError(t, err)
					request := <-launches
					require.Contains(t, request.Arguments, "--resume")
					require.Contains(t, request.Arguments, string(sessionID))
					environment := map[string]string{}
					for _, entry := range request.Environment {
						key, value, _ := strings.Cut(entry, "=")
						environment[key] = value
					}
					require.Equal(t, tc.token, environment["TOOL_TOKEN"])
					require.Equal(t, strings.Join(append(slices.Clone(tc.paths), "/usr/bin:/bin"), string(os.PathListSeparator)), environment["PATH"])
					require.NoError(t, agent.Close())
					require.Equal(t, tc.writes, store.replacements.Load(), "matching and omitted cold recovery must not replace")
					entries, err := store.Load(t.Context(), main)
					require.NoError(t, err)
					require.Len(t, entries, 2)
					configuration, err := unmarshalSessionConfiguration(entries[0])
					require.NoError(t, err)
					require.True(t, maps.Equal(wantedEnv, configuration.Env))
					require.Equal(t, tc.paths, configuration.ExtraPathDirs)
					require.Equal(t, transcript, entries[1])
					preserved, err := store.Load(t.Context(), sub)
					require.NoError(t, err)
					require.Equal(t, subEntries, preserved)
				}
			})
		}
	}
}

func TestColdConfigurationFailureDoesNotPublishOrPartiallyWrite(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"load", "resume"} {
		for _, failure := range []string{"startup", "replace", "publication"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				t.Parallel()
				const sessionID acp.SessionId = "33333333-3333-4333-8333-333333333333"
				main := SessionKey{SessionID: string(sessionID)}
				sub := SessionKey{SessionID: string(sessionID), Subpath: "subagents/worker"}
				store := &faultSessionStore{SessionStore: NewInMemorySessionStore()}
				original := testStoredSessionEntries(t, ClaudeOptions{Env: map[string]string{"TOOL_TOKEN": "stored"}}, []byte(`{"type":"user"}`))
				subEntries := []SessionStoreEntry{[]byte(`{"type":"assistant"}`)}
				require.NoError(t, store.Append(t.Context(), main, original))
				require.NoError(t, store.Append(t.Context(), sub, subEntries))
				transport := newFakeClaudeTransport()
				fault := errors.New("injected " + failure + " failure")
				switch failure {
				case "startup":
					transport.startErr = fault
				case "replace":
					store.replaceErr = fault
				}
				agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store),
					WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
				if failure == "publication" {
					_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
					require.NoError(t, err)
				}
				installFakeClaudeClient(agent, transport)
				t.Cleanup(func() { require.NoError(t, agent.Close()) })
				err := coldConfigurationOperation(t.Context(), agent, operation, sessionID, t.TempDir(),
					map[string]any{"env": map[string]string{"TOOL_TOKEN": "rotated"}})
				if failure == "publication" {
					require.ErrorContains(t, err, `"limit":"active_sessions"`)
				} else {
					require.ErrorIs(t, err, fault)
				}
				require.NotContains(t, agent.sessions, sessionID)
				entries, err := store.Load(t.Context(), main)
				require.NoError(t, err)
				if failure == "publication" {
					require.Len(t, entries, 2)
					configuration, decodeErr := unmarshalSessionConfiguration(entries[0])
					require.NoError(t, decodeErr)
					require.Equal(t, map[string]string{"TOOL_TOKEN": "rotated"}, configuration.Env)
					require.Equal(t, original[1:], entries[1:])
				} else {
					require.Equal(t, original, entries)
				}
				preserved, err := store.Load(t.Context(), sub)
				require.NoError(t, err)
				require.Equal(t, subEntries, preserved)
				if failure != "startup" {
					require.Equal(t, 1, transport.CloseCalls(), "unpublished successor must be contained")
				}
			})
		}
	}
}

type countingConfigurationStore struct {
	SessionStore
	replacements atomic.Int32
}

func (s *countingConfigurationStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	s.replacements.Add(1)

	return s.SessionStore.Replace(ctx, main, replacements)
}

func coldConfigurationOperation(ctx context.Context, agent *Agent, operation string, id acp.SessionId, cwd string, options map[string]any) error {
	meta := WithSessionMeta(map[string]any{"claude": map[string]any{"options": options}})
	if operation == "load" {
		_, err := agent.LoadSession(ctx, LoadSessionRequest(id, cwd, meta))

		return err
	}
	_, err := agent.ResumeSession(ctx, ResumeSessionRequest(id, cwd, meta))

	return err
}

// Use the production client and process transport so assertions observe the
// complete environment delivered to the host's native launch boundary.
func newColdConfigurationNativeAgent(t *testing.T, store SessionStore) (*Agent, <-chan NativeRequest) {
	t.Helper()

	launches := make(chan NativeRequest, 1)
	authority := newFakeHostAuthority()
	authority.start = func(_ context.Context, request NativeRequest) (NativeProcess, error) {
		if request.Executable == keychainToolExecutable {
			return valueNativeProcess{}, nil
		}
		if slices.Equal(request.Arguments, []string{"--version"}) {
			return &fakeNativeProcess{authority: authority, stdout: io.NopCloser(strings.NewReader("2.1.263\n"))}, nil
		}
		launches <- request

		return newConfigurationNativeProcess(), nil
	}
	agent := NewAgent(WithHome(t.TempDir()), WithScratchDir(t.TempDir()), WithExecutablePath("fixture-claude"),
		WithHostAuthority(authority), WithSessionStore(store), WithLogger(slog.New(slog.DiscardHandler)))
	agent.setConnection(newRecordingAgentClient())
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	return agent, launches
}

type configurationNativeProcess struct {
	input  *io.PipeReader
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	output *io.PipeWriter
	done   chan struct{}
}

func newConfigurationNativeProcess() *configurationNativeProcess {
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	process := &configurationNativeProcess{input: input, stdin: stdin, stdout: stdout, output: output, done: make(chan struct{})}
	go func() {
		defer close(process.done)
		defer output.Close()
		defer input.Close()
		fixture := newFakeClaudeTransport()
		decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
		for {
			var request claude.ControlRequest
			if err := decoder.Decode(&request); err != nil {
				return
			}
			fixture.respond(request)
			if err := encoder.Encode(<-fixture.messages); err != nil {
				return
			}
		}
	}()

	return process
}

func (p *configurationNativeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *configurationNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (*configurationNativeProcess) Stderr() io.ReadCloser   { return io.NopCloser(strings.NewReader("")) }

func (p *configurationNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-p.done:
		return NativeResult{}, nil
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (p *configurationNativeProcess) Revoke(context.Context) error {
	return errors.Join(p.input.Close(), p.output.Close())
}
