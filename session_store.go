package claudeacp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	sessionStoreMainSubpath = ""
)

// SessionStoreEntry is one opaque Claude transcript JSON object.
//
// Implementations should preserve the raw JSON bytes. The agent validates
// imported entries as single JSON objects before appending them.
type SessionStoreEntry = json.RawMessage

// SessionKey addresses one Claude transcript in a session store.
//
// A key with an empty Subpath addresses the main session transcript.
// Non-empty subpaths are slash-separated relative paths below the session ID
// directory, without .jsonl or .meta.json suffixes.
type SessionKey struct {
	// ProjectKey is Claude's sanitized project directory key for the session cwd.
	ProjectKey string
	// SessionID is the Claude session UUID or opaque ACP session ID being stored.
	SessionID string
	// Subpath is empty for the main transcript or a slash-separated subagent key.
	Subpath string
}

// SessionSummary is a lightweight entry returned by session-store listers.
type SessionSummary struct {
	// SessionID is the session ID for a main transcript.
	SessionID string
	// MTime is a Unix millisecond timestamp used for session/list ordering.
	MTime int64
}

// SessionStore mirrors Claude transcript entries into a host-provided backend.
//
// Append should be durable before returning. Load must return a copy or
// otherwise immutable entries because callers may retain or trim the returned
// byte slices. Implementations should treat keys as opaque apart from the
// documented main/subpath distinction.
type SessionStore interface {
	Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error
	Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)
}

// SessionStoreLister lists main transcript keys for one Claude project key.
//
// Implement this on a SessionStore to include remote sessions in session/list.
type SessionStoreLister interface {
	ListSessions(ctx context.Context, projectKey string) ([]SessionSummary, error)
}

// SessionStoreSubkeyLister lists subpaths below a main transcript key.
//
// Implement this on a SessionStore to hydrate Claude subagent transcripts and
// metadata during store-backed session/load or session/resume.
type SessionStoreSubkeyLister interface {
	ListSubkeys(ctx context.Context, key SessionKey) ([]string, error)
}

// SessionStoreDeleter deletes one transcript key.
//
// Deleting a main key should also delete all subkeys for that session.
type SessionStoreDeleter interface {
	Delete(ctx context.Context, key SessionKey) error
}

// SessionStoreReplacement is one transcript written during an atomic store replace.
type SessionStoreReplacement struct {
	Key     SessionKey
	Entries []SessionStoreEntry
}

// SessionStoreReplacer atomically replaces one main transcript and its subkeys.
//
// Implement this on a SessionStore that supports replacing existing imported
// sessions. ReplaceSession must leave the previous session intact if it returns
// an error.
type SessionStoreReplacer interface {
	ReplaceSession(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error
}

// InMemorySessionStore is a development and test store for Claude transcripts.
type InMemorySessionStore struct {
	mu      sync.Mutex
	entries map[SessionKey][]SessionStoreEntry
	mtime   map[SessionKey]int64
}

var _ SessionStore = (*InMemorySessionStore)(nil)
var _ SessionStoreLister = (*InMemorySessionStore)(nil)
var _ SessionStoreSubkeyLister = (*InMemorySessionStore)(nil)
var _ SessionStoreDeleter = (*InMemorySessionStore)(nil)
var _ SessionStoreReplacer = (*InMemorySessionStore)(nil)

// NewInMemorySessionStore creates an empty process-local transcript store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{
		entries: make(map[SessionKey][]SessionStoreEntry),
		mtime:   make(map[SessionKey]int64),
	}
}

// Append stores transcript entries under key.
func (s *InMemorySessionStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	if len(entries) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.mtime == nil {
		s.mtime = make(map[SessionKey]int64)
	}

	for _, entry := range entries {
		s.entries[key] = append(s.entries[key], cloneStoreEntry(entry))
	}

	s.mtime[key] = time.Now().UnixMilli()

	return nil
}

// Load returns a copy of transcript entries for key.
func (s *InMemorySessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneStoreEntries(s.entries[key]), nil
}

// ListSessions lists main sessions for one project key.
func (s *InMemorySessionStore) ListSessions(ctx context.Context, projectKey string) ([]SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]SessionSummary, 0)

	for key := range s.entries {
		if key.ProjectKey != projectKey || key.SessionID == "" || key.Subpath != sessionStoreMainSubpath {
			continue
		}

		summaries = append(summaries, SessionSummary{
			SessionID: key.SessionID,
			MTime:     s.mtime[key],
		})
	}

	slices.SortFunc(summaries, func(left, right SessionSummary) int {
		if byTime := cmp.Compare(right.MTime, left.MTime); byTime != 0 {
			return byTime
		}

		return strings.Compare(left.SessionID, right.SessionID)
	})

	return summaries, nil
}

// ListSubkeys lists subpaths below the provided main key.
func (s *InMemorySessionStore) ListSubkeys(ctx context.Context, key SessionKey) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	subkeys := make([]string, 0)

	for candidate := range s.entries {
		if candidate.ProjectKey != key.ProjectKey ||
			candidate.SessionID != key.SessionID ||
			candidate.Subpath == sessionStoreMainSubpath {
			continue
		}

		subkeys = append(subkeys, candidate.Subpath)
	}

	slices.Sort(subkeys)

	return subkeys, nil
}

// Delete removes a transcript key. Deleting the main key cascades to subkeys.
func (s *InMemorySessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for candidate := range s.entries {
		if candidate.ProjectKey != key.ProjectKey || candidate.SessionID != key.SessionID {
			continue
		}

		if key.Subpath != sessionStoreMainSubpath && candidate.Subpath != key.Subpath {
			continue
		}

		delete(s.entries, candidate)
		delete(s.mtime, candidate)
	}

	return nil
}

// ReplaceSession atomically replaces a main transcript and all of its subkeys.
func (s *InMemorySessionStore) ReplaceSession(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	for _, replacement := range replacements {
		if replacement.Key.ProjectKey != main.ProjectKey || replacement.Key.SessionID != main.SessionID {
			return fmt.Errorf("replacement key does not match main session")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.mtime == nil {
		s.mtime = make(map[SessionKey]int64)
	}

	for candidate := range s.entries {
		if candidate.ProjectKey == main.ProjectKey && candidate.SessionID == main.SessionID {
			delete(s.entries, candidate)
			delete(s.mtime, candidate)
		}
	}

	mtime := time.Now().UnixMilli()

	for _, replacement := range replacements {
		if len(replacement.Entries) == 0 {
			continue
		}

		s.entries[replacement.Key] = cloneStoreEntries(replacement.Entries)
		s.mtime[replacement.Key] = mtime
	}

	return nil
}

func cloneStoreEntry(entry SessionStoreEntry) SessionStoreEntry {
	return append(SessionStoreEntry(nil), entry...)
}

func cloneStoreEntries(entries []SessionStoreEntry) []SessionStoreEntry {
	if len(entries) == 0 {
		return nil
	}

	clone := make([]SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		clone = append(clone, cloneStoreEntry(entry))
	}

	return clone
}

func projectKeyForDirectory(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("cwd is required")
	}

	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be an absolute path")
	}

	absolute := filepath.Clean(cwd)

	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = resolved
	}

	return sanitizeSessionProjectPath(filepath.Clean(absolute)), nil
}

func sanitizeSessionProjectPath(path string) string {
	var builder strings.Builder

	for _, char := range path {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}

	if builder.Len() == 0 {
		return "-"
	}

	return builder.String()
}
