package claudeacp

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
)

// sessionRecord is the adapter-owned state a session needs to resume: where
// claude keeps the native file, the listing title, and the configuration the
// session was established with.
type sessionRecord struct {
	SessionID             string            `json:"sessionId"`
	NativeSessionID       string            `json:"nativeSessionId"`
	Title                 string            `json:"title,omitempty"`
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	NativePath            string            `json:"nativePath"`
	Env                   map[string]string `json:"env,omitempty"`
	ExtraPathDirs         []string          `json:"extraPathDirs,omitempty"`
	Model                 string            `json:"model,omitempty"`
	Effort                string            `json:"effort,omitempty"`
	PermissionMode        string            `json:"permissionMode,omitempty"`
	Bare                  bool              `json:"bare,omitempty"`
	SystemPrompt          string            `json:"systemPrompt,omitempty"`
	OutputSchema          map[string]any    `json:"outputSchema,omitempty"`
	OutputStyle           string            `json:"outputStyle,omitempty"`
}

func (s *session) record() sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionRecord{
		SessionID:             string(s.id),
		NativeSessionID:       s.nativeID,
		Title:                 s.title,
		Cwd:                   s.cwd,
		AdditionalDirectories: slices.Clone(s.additionalDirectories),
		NativePath:            s.nativePath,
		Env:                   maps.Clone(s.options.Env),
		ExtraPathDirs:         slices.Clone(s.options.ExtraPathDirs),
		Model:                 s.model,
		Effort:                s.effort,
		PermissionMode:        s.options.PermissionMode,
		Bare:                  s.options.Bare,
		SystemPrompt:          s.options.SystemPrompt,
		OutputSchema:          wire.CloneMap(s.options.OutputSchema),
		OutputStyle:           s.outputStyle,
	}
}

// commitMirror publishes the transcript rows claude appended since the last
// commit with the current session configuration as one durable generation.
// The configuration is sampled under the same lock that orders commits, so a
// later commit never publishes an older record.
func (s *session) commitMirror(ctx context.Context) error {
	if s.ephemeral {
		return nil
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	s.mu.Lock()
	path := s.nativePath
	s.mu.Unlock()

	if path == "" {
		return nil
	}

	commitCtx, finish := s.agent.observe.StartSessionStore(ctx, "commit")
	err := s.mirror.Commit(commitCtx, sessionlog.File(path), s.record())
	finish(err)

	if err != nil {
		return fmt.Errorf("commit session mirror: %w", err)
	}

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
		return storedSession{}, wire.RestoreRefused(ctx, a.log, vendor, sessionID, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

// loadStoredRecord reads only the configuration record, for listing.
func (a *Agent) loadStoredRecord(ctx context.Context, sessionID acp.SessionId) (sessionRecord, bool, error) {
	loadCtx, finish := a.observe.StartSessionStore(ctx, "load_record")

	var record sessionRecord

	found, err := sessionlog.LoadRecord(loadCtx, a.store, string(sessionID), &record)
	if err == nil && found {
		err = record.validate(string(sessionID))
	}

	finish(err)

	return record, found, err
}

func (r sessionRecord) validate(sessionID string) error {
	if uuid.Validate(r.NativeSessionID) != nil || r.SessionID != sessionID || !filepath.IsAbs(r.Cwd) || !filepath.IsAbs(r.NativePath) {
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

// hydrate reconciles the store with claude's own file before a load or
// resume through the session's mirror, which then holds the stored
// generation. An existing native file at least as long as the store wins and
// its newer rows are adopted; a missing or shorter one is materialized from
// the store. A disagreement at a shared position fails the restore. It
// returns the rows the session now holds.
func (s *session) hydrate(ctx context.Context, stored storedSession) ([][]byte, error) {
	rows, err := s.mirror.Hydrate(ctx, s.nativePath, stored.rows, stored.record, func(rows [][]byte) error {
		return claude.ValidateRows(rows, stored.record.NativeSessionID)
	})
	if err != nil {
		return nil, wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
	}

	return rows, nil
}
