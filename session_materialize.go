package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	defaultSessionStoreLoadTimeout = 10 * time.Second
	resumeEntryTypeAgentMetadata   = "agent_metadata"
)

var copyClaudeConfigFiles = copyClaudeConfigFilesImpl
var deleteNativeTranscript = deleteNativeTranscriptImpl

var (
	materializeGlob        = filepath.Glob
	materializeMkdirAll    = os.MkdirAll
	materializeMkdirTemp   = os.MkdirTemp
	materializeReadFile    = os.ReadFile
	materializeRemove      = os.Remove
	materializeRemoveAll   = os.RemoveAll
	materializeStat        = os.Stat
	materializeUserHomeDir = os.UserHomeDir
	materializeWriteFile   = os.WriteFile
)

type materializedSession struct {
	configDir string
	mainPath  string
}

func (m *materializedSession) Close() error {
	if m == nil || m.configDir == "" {
		return nil
	}

	return materializeRemoveAll(m.configDir)
}

func (a *Agent) materializeStoreSession(
	ctx context.Context,
	sessionID string,
	cwd string,
	sourceClaudeHome string,
	env map[string]string,
) (materialized *materializedSession, err error) {
	return a.materializeStoreSessionWithEntries(ctx, sessionID, cwd, sourceClaudeHome, env, nil)
}

func (a *Agent) materializeStoreSessionWithEntries(
	ctx context.Context,
	sessionID string,
	cwd string,
	sourceClaudeHome string,
	env map[string]string,
	storeEntries []SessionStoreEntry,
) (materialized *materializedSession, err error) {
	if sessionID == "" {
		return noMaterializedSession()
	}

	if !validUUIDShape(sessionID) {
		return noMaterializedSession()
	}

	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		return nil, err
	}

	ctx, finishMaterialize := a.observe.StartSessionStore(ctx, "materialize")
	defer func() { finishMaterialize(err) }()

	store := a.sessionStore()
	mainKey := SessionKey{SessionID: sessionID}

	entries := storeEntries
	if entries == nil {
		entries, err = a.loadStoreEntries(ctx, store, mainKey)
		if err != nil {
			return nil, err
		}
	}

	if len(entries) == 0 {
		return noMaterializedSession()
	}

	scratchDir, err := ensureScratchParent(a.options.ScratchDir)
	if err != nil {
		return nil, err
	}

	tmp, err := materializeMkdirTemp(scratchDir, "acp-go-claude-resume-*")
	if err != nil {
		return nil, fmt.Errorf("create temp Claude config dir: %w", err)
	}

	materialized = &materializedSession{configDir: tmp}
	created := materialized

	success := false
	defer func() {
		if !success {
			if cleanupErr := created.Close(); cleanupErr != nil {
				err = errors.Join(err, errors.New("clean up materialized session"))
			}
		}
	}()

	projectDir := filepath.Join(tmp, "projects", projectKey)

	mainPath := filepath.Join(projectDir, sessionID+".jsonl")
	if err := writeStoreJSONL(mainPath, entries); err != nil {
		return nil, err
	}

	if err := copyClaudeConfigFiles(tmp, sourceClaudeHome, env, claudeProcessIsolation(a.options.ProcessIsolation)); err != nil {
		return nil, err
	}

	if err := a.materializeStoreSubkeys(ctx, store, projectDir, mainKey); err != nil {
		return nil, err
	}

	materialized.mainPath = mainPath
	success = true

	return materialized, nil
}

func noMaterializedSession() (*materializedSession, error) {
	return nil, nil //nolint:nilnil // nil materialization is an expected non-error outcome.
}

func (a *Agent) loadStoreEntries(ctx context.Context, store SessionStore, key SessionKey) ([]SessionStoreEntry, error) {
	loadCtx, cancel := context.WithTimeout(ctx, a.sessionStoreLoadTimeout())
	defer cancel()

	loadCtx, finishLoad := a.observe.StartSessionStore(loadCtx, "load")
	entries, err := store.Load(loadCtx, key)
	finishLoad(err)

	if err != nil {
		return nil, fmt.Errorf("load session store key: %w", err)
	}

	return entries, nil
}

func (a *Agent) sessionStoreLoadTimeout() time.Duration {
	if a.options.SessionStoreLoadTimeout > 0 {
		return a.options.SessionStoreLoadTimeout
	}

	return defaultSessionStoreLoadTimeout
}

func (a *Agent) materializeStoreSubkeys(
	ctx context.Context,
	store SessionStore,
	projectDir string,
	mainKey SessionKey,
) error {
	listCtx, cancel := context.WithTimeout(ctx, a.sessionStoreLoadTimeout())
	defer cancel()

	listCtx, finishListSubkeys := a.observe.StartSessionStore(listCtx, "list_subkeys")
	subkeys, err := store.ListSubkeys(listCtx, mainKey)
	finishListSubkeys(err)

	if err != nil {
		return fmt.Errorf("list session store subkeys: %w", err)
	}

	sessionDir := filepath.Join(projectDir, mainKey.SessionID)

	for _, subpath := range subkeys {
		if imageArtifactSubpath(subpath) {
			continue
		}

		if !isSafeSessionSubpath(subpath) {
			continue
		}

		key := SessionKey{SessionID: mainKey.SessionID, Subpath: subpath}

		entries, err := a.loadStoreEntries(ctx, store, key)
		if err != nil {
			return fmt.Errorf("load session store subkey %q: %w", subpath, err)
		}

		if len(entries) == 0 {
			continue
		}

		transcript, metadata := splitSubagentMetadata(entries)

		targetBase := filepath.Join(sessionDir, filepath.FromSlash(subpath))
		if len(transcript) > 0 {
			if err := writeStoreJSONL(targetBase+".jsonl", transcript); err != nil {
				return err
			}
		}

		if metadata != nil {
			if err := writeJSONFile(targetBase+".meta.json", metadata); err != nil {
				return err
			}
		}
	}

	return nil
}

func splitSubagentMetadata(entries []SessionStoreEntry) ([]SessionStoreEntry, map[string]any) {
	transcript := make([]SessionStoreEntry, 0, len(entries))

	var metadata map[string]any

	for _, entry := range entries {
		var obj map[string]any
		if json.Unmarshal(entry, &obj) == nil && obj[jsonFieldType] == resumeEntryTypeAgentMetadata {
			delete(obj, jsonFieldType)
			metadata = obj

			continue
		}

		transcript = append(transcript, entry)
	}

	return transcript, metadata
}

func writeStoreJSONL(path string, entries []SessionStoreEntry) error {
	if err := materializeMkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create transcript dir: %w", err)
	}

	var builder strings.Builder

	for _, entry := range entries {
		trimmed := strings.TrimSpace(string(entry))
		if trimmed == "" {
			continue
		}

		builder.WriteString(trimmed)
		builder.WriteByte('\n')
	}

	if err := materializeWriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}

	return nil
}

func writeJSONFile(path string, value any) error {
	if err := materializeMkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create metadata dir: %w", err)
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}

	return materializeWriteFile(path, append(data, '\n'), 0o600)
}

func copyClaudeConfigFilesImpl(dst string, sourceClaudeHome string, env map[string]string, isolation *claude.ProcessIsolation) error {
	source := sourceClaudeConfigDir(sourceClaudeHome, env)
	if source == "" || filepath.Clean(source) == filepath.Clean(dst) {
		return nil
	}

	if data, err := materializeReadFile(filepath.Join(source, ".claude.json")); err == nil {
		// #nosec G703 -- dst is an agent-created temporary Claude config directory.
		if err := materializeWriteFile(filepath.Join(dst, ".claude.json"), data, 0o600); err != nil {
			return fmt.Errorf("copy Claude config: %w", err)
		}
	}

	return copyClaudeResumeCredential(source, dst, claude.Options{ProcessIsolation: isolation})
}

func sourceClaudeConfigDir(sourceClaudeHome string, env map[string]string) string {
	if env != nil && strings.TrimSpace(env["CLAUDE_CONFIG_DIR"]) != "" {
		return filepath.Clean(env["CLAUDE_CONFIG_DIR"])
	}

	if strings.TrimSpace(sourceClaudeHome) != "" {
		return filepath.Clean(sourceClaudeHome)
	}

	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return filepath.Clean(configDir)
	}

	home, err := materializeUserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".claude")
}

func deleteNativeTranscriptImpl(ctx context.Context, claudeHome string, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if !validUUIDShape(sessionID) {
		return nil
	}

	source := sourceClaudeConfigDir(claudeHome, nil)
	if source == "" {
		return nil
	}

	matches, err := materializeGlob(filepath.Join(source, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return err
	}

	for _, match := range matches {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if removeErr := materializeRemove(match); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}

	sessionDirs, err := materializeGlob(filepath.Join(source, "projects", "*", sessionID))
	if err != nil {
		return err
	}

	for _, dir := range sessionDirs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if removeErr := materializeRemoveAll(dir); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}

	return nil
}
