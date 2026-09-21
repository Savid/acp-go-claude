package claudeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"

	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

// sessionRecord is the adapter-owned state a session needs to resume: where
// claude keeps the native file and the configuration the session was established
// with.
type sessionRecord struct {
	SessionID             string            `json:"sessionId"`
	NativeSessionID       string            `json:"nativeSessionId"`
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	SessionFile           string            `json:"sessionFile"`
	Env                   map[string]string `json:"env,omitempty"`
	ExtraPathDirs         []string          `json:"extraPathDirs,omitempty"`
	Model                 string            `json:"model,omitempty"`
	Effort                string            `json:"effort,omitempty"`
	PermissionMode        string            `json:"permissionMode,omitempty"`
	Bare                  bool              `json:"bare,omitempty"`
	SystemPrompt          string            `json:"systemPrompt,omitempty"`
	OutputSchema          map[string]any    `json:"outputSchema,omitempty"`
	OutputStyle           string            `json:"outputStyle,omitempty"`
	UpdatedAtUnixMilli    int64             `json:"updatedAtUnixMilli"`
}

func (s *session) record() sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionRecord{
		SessionID:             string(s.id),
		NativeSessionID:       s.nativeID,
		Cwd:                   s.cwd,
		AdditionalDirectories: slices.Clone(s.additionalDirectories),
		SessionFile:           s.sessionFile,
		Env:                   maps.Clone(s.options.Env),
		ExtraPathDirs:         slices.Clone(s.options.ExtraPathDirs),
		Model:                 s.model,
		Effort:                s.effort,
		PermissionMode:        s.options.PermissionMode,
		Bare:                  s.options.Bare,
		SystemPrompt:          s.options.SystemPrompt,
		OutputSchema:          wire.CloneMap(s.options.OutputSchema),
		OutputStyle:           s.outputStyle,
		UpdatedAtUnixMilli:    time.Now().UnixMilli(),
	}
}

// commitMirror publishes the native rows and current session configuration
// as one durable generation.
func (s *session) commitMirror(ctx context.Context) error {
	if s.ephemeral {
		return nil
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	s.mu.Lock()
	path := s.sessionFile
	mirrored := s.mirrored
	s.mu.Unlock()

	if path == "" {
		return nil
	}

	rows, err := claude.ReadRows(path)
	if err != nil {
		return fmt.Errorf("read native session: %w", err)
	}

	if len(rows) < mirrored {
		return fmt.Errorf("native log shrank from %d to %d rows", mirrored, len(rows))
	}

	commitCtx, finish := s.agent.observe.StartSessionStore(ctx, "replace")
	err = sessionlog.Commit(commitCtx, s.agent.store, string(s.id), rows, s.record())
	finish(err)

	if err == nil {
		s.mu.Lock()
		s.persisted = true
		s.mu.Unlock()
	}

	if err != nil {
		return fmt.Errorf("commit session mirror: %w", err)
	}

	s.mu.Lock()
	s.mirrored = len(rows)
	s.mu.Unlock()

	return nil
}

// storedSession is what the store holds for one session id.
type storedSession struct {
	rows   [][]byte
	record sessionRecord
	found  bool
}

// loadStored reads the native rows and required current configuration.
func (a *Agent) loadStored(ctx context.Context, sessionID acp.SessionId) (storedSession, error) {
	loadCtx, finish := a.observe.StartSessionStore(ctx, "load")

	var record sessionRecord

	rows, found, err := sessionlog.Load(loadCtx, a.store, string(sessionID), &record)
	if err == nil && found {
		err = record.validate(string(sessionID))
	}

	finish(err)

	if err != nil {
		return storedSession{}, a.restoreRefused(ctx, sessionID, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

func (r sessionRecord) validate(sessionID string) error {
	if uuid.Validate(r.NativeSessionID) != nil || r.SessionID != sessionID || !filepath.IsAbs(r.Cwd) || !filepath.IsAbs(r.SessionFile) || r.UpdatedAtUnixMilli <= 0 {
		return fmt.Errorf("invalid session record identity or location")
	}

	for _, directory := range r.AdditionalDirectories {
		if !filepath.IsAbs(directory) {
			return fmt.Errorf("invalid additional directory")
		}
	}

	if _, err := parseSessionMeta(inheritCarrier(sessionMeta{}, r).Meta()); err != nil {
		return err
	}

	return nil
}

// hydrate reconciles the store with claude's own file before a load or resume. An
// existing native file at least as long as the store wins and its newer rows
// are adopted; a missing or shorter one is materialized from the store. A
// disagreement at a shared position fails the restore. It returns the native
// path and the rows the session now holds.
func (a *Agent) hydrate(ctx context.Context, sessionID acp.SessionId, stored storedSession, cwd string, agentDir string) (string, [][]byte, error) {
	path := claude.SessionPath(agentDir, cwd, stored.record.NativeSessionID)

	native, err := claude.ReadRows(path)
	if err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	rows, nativeWins, err := sessionlog.Reconcile(native, stored.rows)
	if err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	// An empty conversation is a legal stored generation; a non-empty one must
	// be this session's own transcript.
	if len(rows) != 0 {
		if err := claude.ValidateRows(rows, stored.record.NativeSessionID); err != nil {
			return "", nil, a.restoreRefused(ctx, sessionID, err)
		}
	}

	if nativeWins {
		if len(rows) > len(stored.rows) {
			if err := sessionlog.Commit(ctx, a.store, string(sessionID), rows, stored.record); err != nil {
				return "", nil, a.restoreRefused(ctx, sessionID, err)
			}
		}

		return path, rows, nil
	}

	if err := claude.WriteRows(path, rows); err != nil {
		return "", nil, a.restoreRefused(ctx, sessionID, err)
	}

	return path, rows, nil
}

func (a *Agent) restoreRefused(ctx context.Context, sessionID acp.SessionId, err error) error {
	a.log.ErrorContext(ctx, "claude session restore failed",
		slog.String("session_id", string(sessionID)), slog.String("reason", err.Error()))

	return wire.RestoreFailed(vendor)
}

// storedTitle derives a listing title: the first user message text, else the
// session id.
func storedTitle(sessionID string, rows [][]byte) string {
	for _, row := range rows {
		var entry struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}

		if json.Unmarshal(row, &entry) != nil || entry.Type != messageRoleUser {
			continue
		}

		var message claude.Message
		if json.Unmarshal(entry.Message, &message) != nil || message.Role != messageRoleUser {
			continue
		}

		blocks, err := message.ContentBlocks()
		if err != nil {
			continue
		}

		for index := range blocks {
			if blocks[index].Type != contentBlockTypeText {
				continue
			}

			if title := wire.NormalizeTitle(blocks[index].Text); title != "" {
				return title
			}
		}
	}

	return sessionID
}
