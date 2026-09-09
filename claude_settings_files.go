package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	platformDarwin  = "darwin"
	platformLinux   = "linux"
	platformWindows = "windows"

	settingsFileName      = "settings.json"
	settingsLocalFileName = "settings.local.json"
	settingsDirName       = ".claude"

	settingsFieldAvailableModels = "availableModels"
	settingsFieldAPIKeyHelper    = "apiKeyHelper"
	settingsFieldDefaultMode     = "defaultMode"
	settingsFieldEffortLevel     = "effortLevel"
	settingsFieldEnv             = "env"
	settingsFieldModel           = "model"
	settingsFieldPermissions     = "permissions"

	settingsFileOptionField = "settingsFile"
)

var managedSettingsPath = defaultManagedSettingsPath

var (
	filepathAbs          = filepath.Abs
	filepathEvalSymlinks = filepath.EvalSymlinks
	userHomeDir          = os.UserHomeDir
)

type discoveredSettings struct {
	Model          string
	Effort         string
	PermissionMode string
	Env            map[string]string
}

type settingsFile struct {
	Model          string
	Effort         string
	PermissionMode string
	APIKeyHelper   string
	Env            map[string]string
}

func loadDiscoveredSettings(ctx context.Context, cwd string, claudeHome string, log *slog.Logger) discoveredSettings {
	paths := []string{
		userSettingsPath(claudeHome),
		filepath.Join(cwd, settingsDirName, settingsFileName),
		filepath.Join(cwd, settingsDirName, settingsLocalFileName),
		managedSettingsPath(),
	}

	var merged discoveredSettings

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			if log != nil {
				log.DebugContext(ctx, "stop loading Claude settings", slog.String("stage", "settings_discovery"))
			}

			return merged
		}

		settings, ok := loadSettingsFile(ctx, path, log)
		if !ok {
			continue
		}

		if settings.Model != "" {
			merged.Model = settings.Model
		}

		if settings.Effort != "" {
			merged.Effort = settings.Effort
		}

		if settings.PermissionMode != "" {
			merged.PermissionMode = settings.PermissionMode
		}

		if len(settings.Env) > 0 {
			merged.Env = mergeEnv(merged.Env, settings.Env)
		}
	}

	return merged
}

// resolveClaudeSettingsFile resolves a settings-overlay relpath to an absolute
// path under the effective Claude config dir. relpath is confined to dir with
// the same rules as seed files: absolute paths, ".." escapes, and empty keys
// fail closed with the uniform unsupported error naming the offending relpath.
func resolveClaudeSettingsFile(dir string, relpath string) (string, error) {
	if !validSeedFilePath(relpath) {
		return "", settingsFileError(relpath)
	}

	return filepath.Join(dir, filepath.FromSlash(relpath)), nil
}

func settingsFileError(relpath string) error {
	field := settingsFileOptionField
	if relpath != "" {
		field = fmt.Sprintf("%s[%q]", settingsFileOptionField, relpath)
	}

	return unsupportedField(field)
}

func canonicalClaudeHome(path string) (string, error) {
	canonical, _, err := resolveClaudeHome(path)

	return canonical, err
}

// resolveClaudeHome canonicalizes the configured home and reports which
// directory it named. The stat follows every component, so it describes the
// directory the canonical path reaches rather than the name that led there, and
// a caller holding it can tell that directory apart from a later replacement.
func resolveClaudeHome(path string) (string, os.FileInfo, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil, nil
	}

	absolute, err := filepathAbs(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Claude home: %w", err)
	}

	info, err := os.Stat(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("stat Claude home %q: %w", absolute, err)
	}

	if !info.IsDir() {
		return "", nil, fmt.Errorf("claude home %q is not a directory", absolute)
	}

	canonical, err := filepathEvalSymlinks(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("canonicalize Claude home %q: %w", absolute, err)
	}

	return canonical, info, nil
}

func loadSettingsFile(ctx context.Context, path string, log *slog.Logger) (settingsFile, bool) {
	if strings.TrimSpace(path) == "" {
		return settingsFile{}, false
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && log != nil {
			log.DebugContext(ctx, "read Claude settings failed", slog.String("stage", "settings_read"))
		}

		return settingsFile{}, false
	}

	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		if log != nil {
			log.DebugContext(ctx, "decode Claude settings failed", slog.String("stage", "settings_decode"))
		}

		return settingsFile{}, false
	}

	return decodeSettingsFile(ctx, raw, log), true
}

func decodeSettingsFile(ctx context.Context, raw map[string]any, log *slog.Logger) settingsFile {
	settings := settingsFile{
		Model:        stringSetting(raw, settingsFieldModel),
		Effort:       stringSetting(raw, settingsFieldEffortLevel),
		APIKeyHelper: stringSetting(raw, settingsFieldAPIKeyHelper),
		Env:          stringMapSetting(ctx, raw, settingsFieldEnv, log),
	}

	if permissions, _ := raw[settingsFieldPermissions].(map[string]any); permissions != nil {
		settings.PermissionMode = stringSetting(permissions, settingsFieldDefaultMode)
	}

	return settings
}

func userSettingsPath(claudeHome string) string {
	if strings.TrimSpace(claudeHome) != "" {
		return filepath.Join(claudeHome, settingsFileName)
	}

	if configDir := strings.TrimSpace(os.Getenv(claudeConfigDirEnv)); configDir != "" {
		return filepath.Join(configDir, settingsFileName)
	}

	home, err := userHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, settingsDirName, settingsFileName)
}

func defaultManagedSettingsPath() string {
	switch claude.Platform {
	case platformDarwin:
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case platformWindows:
		return `C:\Program Files\ClaudeCode\managed-settings.json`
	default:
		return "/etc/claude-code/managed-settings.json"
	}
}

func stringSetting(raw map[string]any, key string) string {
	value, _ := raw[key].(string)

	return strings.TrimSpace(value)
}

func stringMapSetting(ctx context.Context, raw map[string]any, key string, log *slog.Logger) map[string]string {
	values, _ := raw[key].(map[string]any)
	if len(values) == 0 {
		return nil
	}

	result := make(map[string]string, len(values))
	for key, value := range values {
		text, ok := value.(string)
		if !ok || !validEnvName(key) || strings.ContainsRune(text, '\x00') {
			if log != nil {
				log.DebugContext(ctx, "ignoring invalid settings env entry", slog.String("key", key))
			}

			continue
		}

		result[key] = text
	}

	return result
}

func mergeEnv(base map[string]string, override map[string]string) map[string]string {
	if len(base)+len(override) == 0 {
		return nil
	}

	merged := make(map[string]string, len(base)+len(override))
	for _, source := range []map[string]string{base, override} {
		for _, key := range slices.Sorted(maps.Keys(source)) {
			merged[claude.EnvironmentKey(key)] = source[key]
		}
	}

	return merged
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}
