//go:build integration

package integration

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestClaudeCLISessionStoreMirrorAndResume(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	store := newRecordingSessionStore()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, claudeacp.WithSessionStore(store))

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_STORE_OK and no punctuation.")},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_STORE_OK")

	require.Eventually(t, func() bool {
		return store.hasMainSession(string(session.SessionId))
	}, 30*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	client = &recordingClient{}
	conn = connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, claudeacp.WithSessionStore(store))
	_, err = conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp = promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_STORE_RESUME_OK and no punctuation.")},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_STORE_RESUME_OK")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

type recordingSessionStore struct {
	mu           sync.Mutex
	entries      map[claudeacp.SessionKey][]claudeacp.SessionStoreEntry
	mtime        map[claudeacp.SessionKey]int64
	replaceCalls int
}

var _ claudeacp.SessionStore = (*recordingSessionStore)(nil)
var _ claudeacp.SessionStoreLister = (*recordingSessionStore)(nil)
var _ claudeacp.SessionStoreSubkeyLister = (*recordingSessionStore)(nil)
var _ claudeacp.SessionStoreDeleter = (*recordingSessionStore)(nil)
var _ claudeacp.SessionStoreReplacer = (*recordingSessionStore)(nil)

func newRecordingSessionStore() *recordingSessionStore {
	return &recordingSessionStore{
		entries: make(map[claudeacp.SessionKey][]claudeacp.SessionStoreEntry),
		mtime:   make(map[claudeacp.SessionKey]int64),
	}
}

func (s *recordingSessionStore) Append(
	ctx context.Context,
	key claudeacp.SessionKey,
	entries []claudeacp.SessionStoreEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		s.entries[key] = append(s.entries[key], slices.Clone(entry))
	}
	s.mtime[key] = time.Now().UnixMilli()

	return nil
}

func (s *recordingSessionStore) Load(
	ctx context.Context,
	key claudeacp.SessionKey,
) ([]claudeacp.SessionStoreEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneSessionStoreEntries(s.entries[key]), nil
}

func (s *recordingSessionStore) ListSessions(
	ctx context.Context,
	projectKey string,
) ([]claudeacp.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]claudeacp.SessionSummary, 0)
	for key := range s.entries {
		if key.ProjectKey != projectKey || key.SessionID == "" || key.Subpath != "" {
			continue
		}

		summaries = append(summaries, claudeacp.SessionSummary{
			SessionID: key.SessionID,
			MTime:     s.mtime[key],
		})
	}

	return summaries, nil
}

func (s *recordingSessionStore) ListSubkeys(
	ctx context.Context,
	key claudeacp.SessionKey,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	subkeys := make([]string, 0)
	for candidate := range s.entries {
		if candidate.ProjectKey != key.ProjectKey || candidate.SessionID != key.SessionID || candidate.Subpath == "" {
			continue
		}

		subkeys = append(subkeys, candidate.Subpath)
	}

	return subkeys, nil
}

func (s *recordingSessionStore) Delete(ctx context.Context, key claudeacp.SessionKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.replaceCalls++

	for candidate := range s.entries {
		if candidate.ProjectKey != key.ProjectKey || candidate.SessionID != key.SessionID {
			continue
		}

		if key.Subpath != "" && candidate.Subpath != key.Subpath {
			continue
		}

		delete(s.entries, candidate)
		delete(s.mtime, candidate)
	}

	return nil
}

func (s *recordingSessionStore) ReplaceSession(
	ctx context.Context,
	main claudeacp.SessionKey,
	replacements []claudeacp.SessionStoreReplacement,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.replaceCalls++

	for candidate := range s.entries {
		if candidate.ProjectKey == main.ProjectKey && candidate.SessionID == main.SessionID {
			delete(s.entries, candidate)
			delete(s.mtime, candidate)
		}
	}

	mtime := time.Now().UnixMilli()
	for _, replacement := range replacements {
		if replacement.Key.ProjectKey != main.ProjectKey || replacement.Key.SessionID != main.SessionID {
			continue
		}

		s.entries[replacement.Key] = cloneSessionStoreEntries(replacement.Entries)
		s.mtime[replacement.Key] = mtime
	}

	return nil
}

func (s *recordingSessionStore) replaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.replaceCalls
}

func (s *recordingSessionStore) hasMainSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entries := range s.entries {
		if key.SessionID == sessionID && key.Subpath == "" && len(entries) > 0 {
			return true
		}
	}

	return false
}

func cloneSessionStoreEntries(entries []claudeacp.SessionStoreEntry) []claudeacp.SessionStoreEntry {
	if len(entries) == 0 {
		return nil
	}

	clone := make([]claudeacp.SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		clone = append(clone, slices.Clone(entry))
	}

	return clone
}
