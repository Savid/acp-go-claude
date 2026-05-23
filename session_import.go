package claudeacp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	claudeSessionImportMethod       = "_claude/session/import"
	claudeSessionImportChunkMethod  = "_claude/session/importChunk"
	claudeSessionCommitImportMethod = "_claude/session/commitImport"
	claudeSessionAbortImportMethod  = "_claude/session/abortImport"

	claudeSessionImportFormat = "claude-jsonl"

	maxSessionImportEntries      = 100000
	maxSessionImportChunkEntries = 10000
	maxSessionImportLineBytes    = 10 * 1024 * 1024
	maxSessionImportBytes        = 100 * 1024 * 1024
	sessionImportTTL             = 30 * time.Minute
)

type sessionImport struct {
	ImportID   string
	SessionID  string
	Cwd        string
	ProjectKey string

	entries map[SessionKey][]SessionStoreEntry
	order   []SessionKey
	count   int
	bytes   int

	UpdatedAt time.Time
}

var sessionImportNow = time.Now

type claudeSessionImportParams struct {
	ImportID  string            `json:"importId,omitempty"`
	SessionID string            `json:"sessionId"`
	Cwd       string            `json:"cwd"`
	Format    string            `json:"format,omitempty"`
	Subpath   string            `json:"subpath,omitempty"`
	Offset    int               `json:"offset,omitempty"`
	Entries   []json.RawMessage `json:"entries"`
}

type claudeSessionCommitImportParams struct {
	ImportID string `json:"importId"`
	SHA256   string `json:"sha256,omitempty"`
}

type claudeSessionAbortImportParams struct {
	ImportID string `json:"importId"`
}

func (a *Agent) importClaudeSession(ctx context.Context, params json.RawMessage) (any, error) {
	chunk, err := a.importClaudeSessionChunk(ctx, params)
	if err != nil {
		return nil, err
	}

	importID, _ := chunk[jsonFieldImportID].(string)
	commitParams := json.RawMessage(`{"importId":` + strconv.Quote(importID) + `}`)

	commit, err := a.commitClaudeSessionImport(ctx, commitParams)
	if err != nil {
		return nil, err
	}

	return commit, nil
}

func (a *Agent) importClaudeSessionChunk(ctx context.Context, params json.RawMessage) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var req claudeSessionImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := validateSessionImportRequest(req); err != nil {
		return nil, err
	}

	projectKey, _ := projectKeyForDirectory(req.Cwd)

	clean, bytesAccepted, err := validateSessionImportEntries(req.Entries)
	if err != nil {
		return nil, err
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		importID, err = newUUID()
		if err != nil {
			return nil, err
		}
	}

	now := sessionImportNow()
	key := SessionKey{ProjectKey: projectKey, SessionID: req.SessionID, Subpath: req.Subpath}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.reapStaleSessionImportsLocked(now)

	imp := a.imports[importID]
	if imp == nil {
		if req.Offset != 0 {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldOffset: "must be 0 for a new import"})
		}

		imp = &sessionImport{
			ImportID:   importID,
			SessionID:  req.SessionID,
			Cwd:        req.Cwd,
			ProjectKey: projectKey,
			entries:    make(map[SessionKey][]SessionStoreEntry),
			UpdatedAt:  now,
		}
		a.imports[importID] = imp
	} else if err := imp.validateChunk(req, projectKey); err != nil {
		return nil, err
	}

	if req.Offset != len(imp.entries[key]) {
		return nil, acp.NewInvalidParams(map[string]any{
			jsonFieldOffset: map[string]any{
				"expected": len(imp.entries[key]),
				"got":      req.Offset,
			},
		})
	}

	if imp.count+len(clean) > maxSessionImportEntries {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: "import entry limit exceeded"})
	}

	if imp.bytes+bytesAccepted > maxSessionImportBytes {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldEntries: "import byte limit exceeded"})
	}

	if _, ok := imp.entries[key]; !ok {
		imp.order = append(imp.order, key)
	}

	imp.entries[key] = append(imp.entries[key], clean...)
	imp.count += len(clean)
	imp.bytes += bytesAccepted
	imp.UpdatedAt = now

	return map[string]any{
		jsonFieldImportID: importID,
		acpFieldSessionID: imp.SessionID,
		jsonFieldCwd:      imp.Cwd,
		jsonFieldFormat:   claudeSessionImportFormat,
		jsonFieldOffset:   len(imp.entries[key]),
		jsonFieldEntries:  imp.count,
		"bytes":           imp.bytes,
	}, nil
}

func (a *Agent) commitClaudeSessionImport(ctx context.Context, params json.RawMessage) (any, error) {
	var req claudeSessionCommitImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: validationRequired})
	}

	now := sessionImportNow()

	a.mu.Lock()
	a.reapStaleSessionImportsLocked(now)

	imp := a.imports[importID]
	if imp != nil {
		delete(a.imports, importID)
	}
	a.mu.Unlock()

	if imp == nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: "unknown import"})
	}

	sum := imp.sha256()
	if req.SHA256 != "" && !strings.EqualFold(req.SHA256, sum) {
		return nil, acp.NewInvalidParams(map[string]any{
			jsonFieldSHA256: map[string]any{"expected": req.SHA256, "actual": sum},
		})
	}

	store := a.sessionStore()
	if err := a.replaceStoreImport(ctx, store, imp); err != nil {
		return nil, err
	}

	return map[string]any{
		jsonFieldImportID: imp.ImportID,
		acpFieldSessionID: imp.SessionID,
		jsonFieldCwd:      imp.Cwd,
		jsonFieldFormat:   claudeSessionImportFormat,
		jsonFieldEntries:  imp.count,
		"bytes":           imp.bytes,
		jsonFieldSHA256:   sum,
	}, nil
}

func (a *Agent) abortClaudeSessionImport(_ context.Context, params json.RawMessage) (any, error) {
	var req claudeSessionAbortImportParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	importID := strings.TrimSpace(req.ImportID)
	if importID == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldImportID: validationRequired})
	}

	now := sessionImportNow()

	a.mu.Lock()
	a.reapStaleSessionImportsLocked(now)
	_, existed := a.imports[importID]
	delete(a.imports, importID)
	a.mu.Unlock()

	return map[string]any{"aborted": existed}, nil
}

func (a *Agent) reapStaleSessionImportsLocked(now time.Time) {
	for importID, imp := range a.imports {
		if imp == nil || (!imp.UpdatedAt.IsZero() && now.Sub(imp.UpdatedAt) > sessionImportTTL) {
			delete(a.imports, importID)
		}
	}
}

func validateSessionImportRequest(req claudeSessionImportParams) error {
	if req.SessionID == "" {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: validationRequired})
	}

	if !validUUIDShape(req.SessionID) {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: "must be a UUID"})
	}

	if err := validateRequiredAbsolutePath(jsonFieldCwd, req.Cwd); err != nil {
		return err
	}

	if req.Format != "" && req.Format != claudeSessionImportFormat {
		return acp.NewInvalidParams(map[string]any{jsonFieldFormat: fmt.Sprintf("must be %q", claudeSessionImportFormat)})
	}

	if req.Subpath != "" && !isSafeSessionSubpath(req.Subpath) {
		return acp.NewInvalidParams(map[string]any{jsonFieldSubpath: "must be a relative session subpath"})
	}

	if req.Offset < 0 {
		return acp.NewInvalidParams(map[string]any{jsonFieldOffset: "must be non-negative"})
	}

	if len(req.Entries) == 0 {
		return acp.NewInvalidParams(map[string]any{jsonFieldEntries: validationRequired})
	}

	if len(req.Entries) > maxSessionImportChunkEntries {
		return acp.NewInvalidParams(map[string]any{jsonFieldEntries: "chunk entry limit exceeded"})
	}

	return nil
}

func validateSessionImportEntries(entries []json.RawMessage) ([]SessionStoreEntry, int, error) {
	clean := make([]SessionStoreEntry, 0, len(entries))
	total := 0

	for i, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: validationRequired}})
		}

		if len(trimmed) > maxSessionImportLineBytes {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "line byte limit exceeded"}})
		}

		var obj map[string]json.RawMessage

		dec := json.NewDecoder(bytes.NewReader(trimmed))
		if err := dec.Decode(&obj); err != nil {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: err.Error()}})
		}

		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "must contain one JSON object"}})
		}

		if obj == nil {
			return nil, 0, acp.NewInvalidParams(map[string]any{jsonFieldEntries: map[string]any{jsonFieldIndex: i, jsonFieldError: "must be a JSON object"}})
		}

		clean = append(clean, append(SessionStoreEntry(nil), trimmed...))
		total += len(trimmed) + 1
	}

	return clean, total, nil
}

func (imp *sessionImport) validateChunk(req claudeSessionImportParams, projectKey string) error {
	if imp.SessionID != req.SessionID {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: "does not match existing import"})
	}

	if imp.Cwd != req.Cwd || imp.ProjectKey != projectKey {
		return acp.NewInvalidParams(map[string]any{jsonFieldCwd: "does not match existing import"})
	}

	return nil
}

func (imp *sessionImport) sha256() string {
	hash := sha256.New()

	for _, key := range imp.order {
		for _, entry := range imp.entries[key] {
			_, _ = hash.Write(bytes.TrimSpace(entry))
			_, _ = hash.Write([]byte{'\n'})
		}
	}

	return hex.EncodeToString(hash.Sum(nil))
}

func (a *Agent) replaceStoreImport(ctx context.Context, store SessionStore, imp *sessionImport) error {
	mainKey := SessionKey{ProjectKey: imp.ProjectKey, SessionID: imp.SessionID}

	loadCtx, finishLoad := a.observe.StartSessionStore(ctx, "load_existing")
	existing, err := store.Load(loadCtx, mainKey)
	finishLoad(err)

	if err != nil {
		return fmt.Errorf("load existing imported session: %w", err)
	}

	if len(existing) > 0 {
		replacer, ok := store.(SessionStoreReplacer)
		if !ok {
			return acp.NewInvalidParams(map[string]any{acpFieldSessionID: "session already exists and store does not support atomic replacement"})
		}

		replaceCtx, finishReplace := a.observe.StartSessionStore(ctx, "replace_import")
		err := replacer.ReplaceSession(replaceCtx, mainKey, sessionImportReplacements(imp))
		finishReplace(err)

		if err != nil {
			return fmt.Errorf("replace existing imported session: %w", err)
		}

		return nil
	}

	for _, key := range imp.order {
		appendCtx, finishAppend := a.observe.StartSessionStore(ctx, "append_import")
		err := store.Append(appendCtx, key, imp.entries[key])
		finishAppend(err)

		if err != nil {
			return fmt.Errorf("append imported session: %w", err)
		}
	}

	return nil
}

func sessionImportReplacements(imp *sessionImport) []SessionStoreReplacement {
	replacements := make([]SessionStoreReplacement, 0, len(imp.order))
	for _, key := range imp.order {
		replacements = append(replacements, SessionStoreReplacement{
			Key:     key,
			Entries: cloneStoreEntries(imp.entries[key]),
		})
	}

	return replacements
}

func (a *Agent) sessionStore() SessionStore {
	if a.options.SessionStore != nil {
		return a.options.SessionStore
	}

	return a.importStore
}

func validUUIDShape(value string) bool {
	if len(value) != 36 {
		return false
	}

	for i, char := range value {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !isUUIDHex(char) {
				return false
			}
		}
	}

	return true
}

func isUUIDHex(char rune) bool {
	return (char >= '0' && char <= '9') ||
		(char >= 'a' && char <= 'f') ||
		(char >= 'A' && char <= 'F')
}

func isSafeSessionSubpath(subpath string) bool {
	if subpath == "" ||
		filepath.IsAbs(subpath) ||
		strings.HasPrefix(subpath, "/") ||
		strings.HasPrefix(subpath, "\\") ||
		strings.Contains(subpath, "\x00") ||
		filepath.VolumeName(subpath) != "" {
		return false
	}

	for _, part := range strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return false
		}
	}

	return true
}
