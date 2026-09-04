package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionKeyForMirrorPath(t *testing.T) {
	t.Parallel()

	projectsDir := filepath.Join(t.TempDir(), "projects")
	sessionID := "44444444-4444-4444-8444-444444444444"

	tests := []struct {
		name    string
		path    string
		project string
		want    *SessionKey
	}{
		{
			name:    "main transcript",
			path:    filepath.Join(projectsDir, "project", sessionID+".jsonl"),
			project: projectsDir,
			want:    &SessionKey{SessionID: sessionID},
		},
		{
			name:    "subagent transcript",
			path:    filepath.Join(projectsDir, "project", sessionID, "subagents", "worker", "turn.jsonl"),
			project: projectsDir,
			want:    &SessionKey{SessionID: sessionID, Subpath: "subagents/worker/turn"},
		},
		{
			name:    "subagent metadata",
			path:    filepath.Join(projectsDir, "project", sessionID, "subagents", "worker", "turn.meta.json"),
			project: projectsDir,
			want:    &SessionKey{SessionID: sessionID, Subpath: "subagents/worker/turn"},
		},
		{name: "empty path", path: "", project: projectsDir},
		{name: "relative path", path: "project/session.jsonl", project: projectsDir},
		{name: "relative projects dir", path: filepath.Join(projectsDir, "project", sessionID+".jsonl"), project: "relative"},
		{name: "outside projects dir", path: filepath.Join(t.TempDir(), sessionID+".jsonl"), project: projectsDir},
		{name: "short path", path: filepath.Join(projectsDir, "project"), project: projectsDir},
		{name: "bad main uuid", path: filepath.Join(projectsDir, "project", "not-a-uuid.jsonl"), project: projectsDir},
		{name: "not subagent", path: filepath.Join(projectsDir, "project", sessionID, "tools", "one.jsonl"), project: projectsDir},
		{name: "bad subagent uuid", path: filepath.Join(projectsDir, "project", "not-a-uuid", "subagents", "one.jsonl"), project: projectsDir},
		{name: "bad extension", path: filepath.Join(projectsDir, "project", sessionID, "subagents", "one.txt"), project: projectsDir},
		{name: "unsafe subpath", path: filepath.Join(projectsDir, "project", sessionID, "subagents", "bad:name.jsonl"), project: projectsDir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sessionKeyForMirrorPath(tt.path, tt.project)
			if tt.want == nil {
				require.Error(t, err)
				require.Nil(t, got)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSessionMirrorAppendFrameAndEntries(t *testing.T) {
	ctx := context.Background()
	sessionID := "55555555-5555-4555-8555-555555555555"
	home := t.TempDir()
	projectsDir := filepath.Join(home, "projects")
	store := NewInMemorySessionStore()
	mirror := &sessionMirror{log: slog.Default(), store: store, projectsDir: projectsDir}

	err := mirror.appendFrame(ctx, &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(projectsDir, "project", sessionID+".jsonl"),
		Entries:  []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	})
	require.NoError(t, err)
	entries, err := store.Load(ctx, SessionKey{SessionID: sessionID})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"user"}`, string(entries[0]))

	require.NoError(t, mirror.appendFrame(ctx, &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(t.TempDir(), sessionID+".jsonl"),
		Entries:  []SessionStoreEntry{[]byte(`{"type":"dropped"}`)},
	}))

	appendErr := errors.New("append failed")
	mirror.store = &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: appendErr}
	err = mirror.appendFrame(ctx, &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(projectsDir, "project", sessionID+".jsonl"),
		Entries:  []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	})
	require.ErrorIs(t, err, errSessionMirrorAppend)
	require.ErrorContains(t, err, "append failed")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = appendMirrorEntries(cancelled, &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: appendErr}, SessionKey{SessionID: sessionID}, []SessionStoreEntry{[]byte(`{}`)})
	require.ErrorIs(t, err, context.Canceled)

	previousAppendTimeout := sessionMirrorAppendTimeout
	sessionMirrorAppendTimeout = time.Nanosecond
	t.Cleanup(func() { sessionMirrorAppendTimeout = previousAppendTimeout })
	err = appendMirrorEntries(ctx, blockingAppendStore{}, SessionKey{SessionID: sessionID}, []SessionStoreEntry{[]byte(`{}`)})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSessionMirrorRejectsForeignMainAndSubagentTranscripts(t *testing.T) {
	t.Parallel()

	const (
		ownerID   = "55555555-5555-4555-8555-555555555555"
		foreignID = "66666666-6666-4666-8666-666666666666"
	)
	home := t.TempDir()
	store := NewInMemorySessionStore()
	session := &agentSession{id: ownerID, configuration: sessionConfiguration{
		Env: map[string]string{"SHOULD_NOT": "BE_WRITTEN"},
	}}
	mirror := newSessionMirror(nil, store, home, session)

	for _, filePath := range []string{
		filepath.Join(home, "projects", "workspace", foreignID+".jsonl"),
		filepath.Join(home, "projects", "workspace", foreignID, "subagents", "worker", "turn.jsonl"),
	} {
		err := mirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{
			FilePath: filePath,
			Entries:  []SessionStoreEntry{[]byte(`{"type":"user"}`)},
		})
		require.ErrorIs(t, err, errSessionMirrorOwner)
	}

	entries, err := store.Load(t.Context(), SessionKey{SessionID: foreignID})
	require.NoError(t, err)
	require.Empty(t, entries)
	require.False(t, mirror.configurationWritten)
}

func TestNewSessionMirrorAndDefaultClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	home := t.TempDir()
	mirror := newSessionMirror(nil, NewInMemorySessionStore(), home, nil)
	require.Equal(t, filepath.Join(home, "projects"), mirror.projectsDir)
	require.NotNil(t, mirror.log)

	envHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", envHome)
	require.Equal(t, filepath.Clean(envHome), defaultClaudeConfigDir(""))

	userHome, err := os.UserHomeDir()
	require.NoError(t, err)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.Equal(t, filepath.Join(userHome, ".claude"), defaultClaudeConfigDir(""))
}

type blockingAppendStore struct{}

func (blockingAppendStore) Append(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
	<-ctx.Done()

	return ctx.Err()
}

func (blockingAppendStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func (blockingAppendStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return nil
}

func (blockingAppendStore) Delete(context.Context, SessionKey) error {
	return nil
}

func (blockingAppendStore) ListSessions(context.Context) ([]SessionSummary, error) {
	return nil, nil
}

func (blockingAppendStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, nil
}
