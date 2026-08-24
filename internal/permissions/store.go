package permissions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	appDirName                = "acp-go-claude"
	permissionsFile           = "session-permissions.json"
	permissionLockRetryBase   = 10 * time.Millisecond
	permissionLockRetryJitter = 5 * time.Millisecond
)

// fileLocks shard in-process locks by session path. Collisions only serialize
// unrelated sessions; OS file locks still protect cross-process writers.
var fileLocks [64]sync.Mutex
var permissionLockJitterSeq atomic.Uint64

type tempFile interface {
	io.WriteCloser
	Name() string
	Sync() error
}

var (
	storeMkdirAll      = os.MkdirAll
	storeMarshalIndent = json.MarshalIndent
	storeCreateTemp    = func(dir string, pattern string) (tempFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	storeOpenDir = func(dir string) (syncCloser, error) {
		return os.Open(dir)
	}
	storeRename      = os.Rename
	storeUserHomeDir = os.UserHomeDir
)

type syncCloser interface {
	io.Closer
	Sync() error
}

// Store persists session-scoped Claude permission rules.
type Store struct {
	ClaudeHome string
}

// Load returns permission rules for a session.
func (s Store) Load(ctx context.Context, sessionID string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	unlock, err := lockPermissionsFile(ctx, s.path())
	if err != nil {
		return nil, err
	}
	defer unlock()

	all, err := s.readAll(ctx)
	if err != nil {
		return nil, err
	}

	return Clone(all[sessionID]), nil
}

// Save replaces the persisted permission rules for a session.
func (s Store) Save(ctx context.Context, sessionID string, rules map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session ID is required")
	}

	unlock, err := lockPermissionsFile(ctx, s.path())
	if err != nil {
		return err
	}
	defer unlock()

	all, err := s.readAll(ctx)
	if err != nil {
		return err
	}

	if len(rules) == 0 {
		delete(all, sessionID)
	} else {
		all[sessionID] = Clone(rules)
	}

	return s.writeAll(all)
}

func lockPermissionsFile(ctx context.Context, path string) (func(), error) {
	localUnlock, err := lockLocalPermissionsFile(ctx, path)
	if err != nil {
		return nil, err
	}

	osUnlock, err := lockOSPermissionsFile(ctx, path+".lock")
	if err != nil {
		localUnlock()

		return nil, err
	}

	return func() {
		// The local lock only protects this process. Keep it held until the OS
		// lock is released so another goroutine cannot enter before peers can.
		osUnlock()
		localUnlock()
	}, nil
}

func lockLocalPermissionsFile(ctx context.Context, path string) (func(), error) {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(path))

	mu := &fileLocks[hash.Sum64()%uint64(len(fileLocks))]
	if mu.TryLock() {
		return mu.Unlock, nil
	}

	for {
		if err := waitPermissionLockRetry(ctx); err != nil {
			return nil, err
		}

		if mu.TryLock() {
			return mu.Unlock, nil
		}
	}
}

func waitPermissionLockRetry(ctx context.Context) error {
	timer := time.NewTimer(permissionLockRetryDelay())
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func permissionLockRetryDelay() time.Duration {
	seq := permissionLockJitterSeq.Add(1)
	jitter := time.Duration((seq * 1103515245) % uint64(permissionLockRetryJitter))

	return permissionLockRetryBase + jitter
}

func (s Store) readAll(ctx context.Context) (map[string]map[string]string, error) {
	path := s.path()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]map[string]string), nil
	}

	if err != nil {
		return nil, fmt.Errorf("read permission rules: %w", err)
	}

	var all map[string]map[string]string
	if err := json.Unmarshal(data, &all); err != nil {
		_, backupErr := backupCorruptPermissionRules(path)
		if backupErr != nil {
			return nil, fmt.Errorf("backup corrupt permission rules after decode failure: %w", backupErr)
		}

		slog.WarnContext(
			ctx,
			"ignoring corrupt permission rules",
			slog.String("stage", "permission_rules_decode"),
		)

		return make(map[string]map[string]string), nil
	}

	if all == nil {
		all = make(map[string]map[string]string)
	}

	return all, nil
}

func backupCorruptPermissionRules(path string) (string, error) {
	backup := path + ".bak"
	if err := storeRename(path, backup); err != nil {
		return "", err
	}

	return backup, nil
}

func (s Store) writeAll(all map[string]map[string]string) error {
	path := s.path()
	if err := storeMkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create permission rules dir: %w", err)
	}

	data, err := storeMarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("encode permission rules: %w", err)
	}

	tmp, err := storeCreateTemp(filepath.Dir(path), ".session-permissions-*.json")
	if err != nil {
		return fmt.Errorf("create permission rules temp file: %w", err)
	}

	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write permission rules temp file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("sync permission rules temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close permission rules temp file: %w", err)
	}

	if err := storeRename(tmpName, path); err != nil {
		return fmt.Errorf("replace permission rules file: %w", err)
	}

	if err := syncPermissionsDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync permission rules dir: %w", err)
	}

	return nil
}

func syncPermissionsDir(dir string) error {
	opened, err := storeOpenDir(dir)
	if err != nil {
		return err
	}
	defer opened.Close()

	return opened.Sync()
}

func (s Store) path() string {
	return filepath.Join(s.configHome(), appDirName, permissionsFile)
}

func (s Store) configHome() string {
	if strings.TrimSpace(s.ClaudeHome) != "" {
		return filepath.Clean(s.ClaudeHome)
	}

	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return filepath.Clean(configDir)
	}

	home, err := storeUserHomeDir()
	if err != nil {
		return filepath.Clean(".claude")
	}

	return filepath.Join(home, ".claude")
}

// Clone returns a defensive copy of permission rules.
func Clone(rules map[string]string) map[string]string {
	if len(rules) == 0 {
		return make(map[string]string)
	}

	clone := make(map[string]string, len(rules))
	maps.Copy(clone, rules)

	return clone
}
