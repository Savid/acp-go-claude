package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	defaultSessionMirrorAppendTimeout = 60 * time.Second
	// defaultSessionMirrorCommitTimeout bounds one commit boundary. It bounds the
	// wait for a queued prefix to land, never how long the adapter listens for
	// more frames: what a boundary covers is decided by arrival order, not by a
	// clock.
	defaultSessionMirrorCommitTimeout = 90 * time.Second
)

var (
	errSessionMirrorAppend = errors.New("append transcript mirror entries")
	errSessionMirrorOwner  = errors.New("transcript mirror session does not match owner")
)

var (
	sessionMirrorAppendTimeout = defaultSessionMirrorAppendTimeout
	sessionSettlementTimeout   = defaultSessionMirrorCommitTimeout
)

type sessionMirror struct {
	log         *slog.Logger
	store       SessionStore
	projectsDir string
	session     *agentSession

	configurationMu      sync.Mutex
	configuration        sessionConfiguration
	configurationWritten bool
}

func newSessionMirror(log *slog.Logger, store SessionStore, claudeHome string, session *agentSession) *sessionMirror {
	if log == nil {
		log = slog.Default()
	}

	mirror := &sessionMirror{
		log:         log.With(slog.String("component", "session_mirror")),
		store:       store,
		projectsDir: filepath.Join(defaultClaudeConfigDir(claudeHome), "projects"),
		session:     session,
	}
	if session != nil {
		mirror.configuration = session.configuration
		mirror.configurationWritten = session.configurationStored
	}

	return mirror
}

// appendFrame writes a transcript mirror frame to the session store. Callers
// guarantee the mirror has a store configured and the frame carries entries; a
// frame whose path falls outside the Claude projects dir is logged and dropped.
func (m *sessionMirror) appendFrame(ctx context.Context, frame *claude.TranscriptMirrorMessage) error {
	key, err := sessionKeyForMirrorPath(frame.FilePath, m.projectsDir)
	if err != nil {
		m.log.WarnContext(ctx, "dropping transcript mirror frame",
			slog.String("stage", "mirror_path_validation"),
			slog.String("class", safeErrorClass(err)))

		return nil
	}

	if m.session != nil && key.SessionID != string(m.session.id) {
		return fmt.Errorf("%w: transcript %q, owner %q", errSessionMirrorOwner, key.SessionID, m.session.id)
	}

	entries := frame.Entries
	if m.session != nil {
		entries, err = m.session.sanitizeTranscriptImageEntries(ctx, entries)
		if err != nil {
			return err
		}
	}

	if key.Subpath == SessionStoreMainSubpath && m.session != nil {
		m.configurationMu.Lock()
		defer m.configurationMu.Unlock()

		if !m.configurationWritten {
			configurationEntry := marshalSessionConfiguration(m.configuration)
			entries = append([]SessionStoreEntry{configurationEntry}, entries...)
		}
	}

	if err := appendMirrorEntries(ctx, m.store, *key, entries); err != nil {
		return fmt.Errorf("%w: %w", errSessionMirrorAppend, err)
	}

	if key.Subpath == SessionStoreMainSubpath && m.session != nil {
		m.configurationWritten = true
	}

	return nil
}

func appendMirrorEntries(ctx context.Context, store SessionStore, key SessionKey, entries []SessionStoreEntry) error {
	var lastErr error

	for _, delay := range []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond} {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		appendCtx, cancel := context.WithTimeout(ctx, sessionMirrorAppendTimeout)
		err := store.Append(appendCtx, key, entries)

		cancel()

		if err == nil {
			return nil
		}

		lastErr = err

		if appendCtx.Err() == context.DeadlineExceeded {
			break
		}
	}

	return lastErr
}

func sessionKeyForMirrorPath(filePath string, projectsDir string) (*SessionKey, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, fmt.Errorf("filePath is required")
	}

	if !filepath.IsAbs(filePath) {
		return nil, fmt.Errorf("filePath must be absolute")
	}

	if !filepath.IsAbs(projectsDir) {
		return nil, fmt.Errorf("projects dir must be absolute")
	}

	absoluteFile := filepath.Clean(filePath)
	absoluteProjects := filepath.Clean(projectsDir)

	rel, _ := filepath.Rel(absoluteProjects, absoluteFile)
	if rel == "." || rel == parentDirSegment ||
		strings.HasPrefix(rel, parentDirSegment+string(filepath.Separator)) {
		return nil, fmt.Errorf("path is outside Claude projects dir")
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || parts[0] == "" {
		return nil, fmt.Errorf("path is not a Claude session transcript")
	}

	if strings.HasSuffix(parts[1], ".jsonl") && len(parts) == 2 {
		sessionID := strings.TrimSuffix(parts[1], ".jsonl")
		if !validUUIDShape(sessionID) {
			return nil, fmt.Errorf("session ID is not a UUID")
		}

		return &SessionKey{SessionID: sessionID}, nil
	}

	if len(parts) < 4 || parts[2] != "subagents" {
		return nil, fmt.Errorf("path is not a supported Claude subagent transcript")
	}

	sessionID := parts[1]
	if !validUUIDShape(sessionID) {
		return nil, fmt.Errorf("session ID is not a UUID")
	}

	last := parts[len(parts)-1]
	switch {
	case strings.HasSuffix(last, ".jsonl"):
		parts[len(parts)-1] = strings.TrimSuffix(last, ".jsonl")
	case strings.HasSuffix(last, ".meta.json"):
		parts[len(parts)-1] = strings.TrimSuffix(last, ".meta.json")
	default:
		return nil, fmt.Errorf("path is not a JSONL or metadata transcript")
	}

	subpath := path.Join(parts[2:]...)
	if !isSafeSessionSubpath(subpath) {
		return nil, fmt.Errorf("unsafe session subpath")
	}

	return &SessionKey{SessionID: sessionID, Subpath: subpath}, nil
}

func defaultClaudeConfigDir(claudeHome string) string {
	if strings.TrimSpace(claudeHome) != "" {
		return filepath.Clean(claudeHome)
	}

	if configDir := strings.TrimSpace(os.Getenv(claudeConfigDirEnv)); configDir != "" {
		return filepath.Clean(configDir)
	}

	home, err := materializeUserHomeDir()
	if err != nil {
		return filepath.Clean(".claude")
	}

	return filepath.Join(home, ".claude")
}
