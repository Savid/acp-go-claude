package claudeacp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	sessionMirrorAppendTimeout = 60 * time.Second
	sessionMirrorDrainTimeout  = 150 * time.Millisecond
)

type sessionMirror struct {
	log         *slog.Logger
	store       SessionStore
	projectsDir string
}

func newSessionMirror(log *slog.Logger, store SessionStore, claudeHome string) *sessionMirror {
	if store == nil {
		return nil
	}

	if log == nil {
		log = slog.Default()
	}

	return &sessionMirror{
		log:         log.With(slog.String("component", "session_mirror")),
		store:       store,
		projectsDir: filepath.Join(defaultClaudeConfigDir(claudeHome), "projects"),
	}
}

func (m *sessionMirror) handle(ctx context.Context, msg claude.Message) (bool, error) {
	frame, ok := msg.(*claude.TranscriptMirrorMessage)
	if !ok {
		return false, nil
	}

	if m == nil || len(frame.Entries) == 0 {
		return true, nil
	}

	key, err := sessionKeyForMirrorPath(frame.FilePath, m.projectsDir)
	if err != nil {
		m.log.WarnContext(ctx, "dropping transcript mirror frame", slog.String("path", frame.FilePath), slog.String(jsonFieldError, err.Error()))

		return true, nil
	}

	if err := appendMirrorEntries(ctx, m.store, *key, frame.Entries); err != nil {
		return true, fmt.Errorf("append transcript mirror entries: %w", err)
	}

	return true, nil
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
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path is outside Claude projects dir")
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || parts[0] == "" {
		return nil, fmt.Errorf("path is not a Claude session transcript")
	}

	projectKey := parts[0]
	if strings.HasSuffix(parts[1], ".jsonl") && len(parts) == 2 {
		sessionID := strings.TrimSuffix(parts[1], ".jsonl")
		if !validUUIDShape(sessionID) {
			return nil, fmt.Errorf("session ID is not a UUID")
		}

		return &SessionKey{ProjectKey: projectKey, SessionID: sessionID}, nil
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

	return &SessionKey{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath}, nil
}

func defaultClaudeConfigDir(claudeHome string) string {
	if strings.TrimSpace(claudeHome) != "" {
		return filepath.Clean(claudeHome)
	}

	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return filepath.Clean(configDir)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Clean(".claude")
	}

	return filepath.Join(home, ".claude")
}
