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
	"runtime"
	"strings"
)

const (
	platformDarwin  = "darwin"
	platformLinux   = "linux"
	platformWindows = "windows"

	settingsFileName      = "settings.json"
	settingsLocalFileName = "settings.local.json"
	settingsDirName       = ".claude"

	settingsFieldAvailableModels = "availableModels"
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
	runtimeGOOS          = runtime.GOOS
	userHomeDir          = os.UserHomeDir
)

type discoveredSettings struct {
	Model              string
	Effort             string
	PermissionMode     string
	AvailableModels    []string
	HasAvailableModels bool
	Env                map[string]string
}

type settingsFile struct {
	Model              string
	Effort             string
	PermissionMode     string
	AvailableModels    []string
	HasAvailableModels bool
	Env                map[string]string
}

func loadDiscoveredSettings(ctx context.Context, cwd string, claudeHome string, log *slog.Logger) discoveredSettings {
	paths := []string{
		userSettingsPath(claudeHome),
		filepath.Join(cwd, settingsDirName, settingsFileName),
		filepath.Join(cwd, settingsDirName, settingsLocalFileName),
		managedSettingsPath(),
	}

	var merged discoveredSettings

	seenModels := make(map[string]struct{})

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			if log != nil {
				log.DebugContext(ctx, "stop loading Claude settings", slog.String(jsonFieldError, err.Error()))
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

		if settings.HasAvailableModels {
			merged.HasAvailableModels = true

			for _, model := range settings.AvailableModels {
				if _, ok := seenModels[model]; ok {
					continue
				}

				merged.AvailableModels = append(merged.AvailableModels, model)
				seenModels[model] = struct{}{}
			}
		}

		if len(settings.Env) > 0 {
			if merged.Env == nil {
				merged.Env = make(map[string]string, len(settings.Env))
			}

			maps.Copy(merged.Env, settings.Env)
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
			log.DebugContext(ctx, "read Claude settings failed", slog.String("path", path), slog.String(jsonFieldError, err.Error()))
		}

		return settingsFile{}, false
	}

	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		if log != nil {
			log.DebugContext(ctx, "decode Claude settings failed", slog.String("path", path), slog.String(jsonFieldError, err.Error()))
		}

		return settingsFile{}, false
	}

	return decodeSettingsFile(ctx, raw, log), true
}

func decodeSettingsFile(ctx context.Context, raw map[string]any, log *slog.Logger) settingsFile {
	settings := settingsFile{
		Model:  stringSetting(raw, settingsFieldModel),
		Effort: stringSetting(raw, settingsFieldEffortLevel),
		Env:    stringMapSetting(ctx, raw, settingsFieldEnv, log),
	}

	if permissions, _ := raw[settingsFieldPermissions].(map[string]any); permissions != nil {
		settings.PermissionMode = stringSetting(permissions, settingsFieldDefaultMode)
	}

	if _, ok := raw[settingsFieldAvailableModels]; ok {
		settings.HasAvailableModels = true
		settings.AvailableModels = stringSliceSetting(raw, settingsFieldAvailableModels)
	}

	return settings
}

func userSettingsPath(claudeHome string) string {
	if strings.TrimSpace(claudeHome) != "" {
		return filepath.Join(claudeHome, settingsFileName)
	}

	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return filepath.Join(configDir, settingsFileName)
	}

	home, err := userHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, settingsDirName, settingsFileName)
}

func defaultManagedSettingsPath() string {
	switch runtimeGOOS {
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

func stringSliceSetting(raw map[string]any, key string) []string {
	values, _ := raw[key].([]any)

	result := make([]string, 0, len(values))
	for _, value := range values {
		text, _ := value.(string)

		text = strings.TrimSpace(text)
		if text != "" {
			result = append(result, text)
		}
	}

	return result
}

func stringMapSetting(ctx context.Context, raw map[string]any, key string, log *slog.Logger) map[string]string {
	values, _ := raw[key].(map[string]any)
	if len(values) == 0 {
		return nil
	}

	result := make(map[string]string, len(values))
	for key, value := range values {
		text, _ := value.(string)

		if !validSettingsEnvName(key) {
			if log != nil {
				log.DebugContext(ctx, "ignoring invalid settings env key", slog.String("key", key))
			}

			continue
		}

		if text != "" && !strings.ContainsRune(text, '\x00') {
			result[key] = text
		}
	}

	return result
}

func validSettingsEnvName(key string) bool {
	if key == "" {
		return false
	}

	for index, char := range key {
		if char == '_' || (char >= 'A' && char <= 'Z') || (index > 0 && char >= '0' && char <= '9') {
			continue
		}

		return false
	}

	return true
}

func mergeEnv(base map[string]string, override map[string]string) map[string]string {
	if len(base) == 0 {
		return cloneStringMap(override)
	}

	merged := cloneStringMap(base)
	maps.Copy(merged, override)

	return merged
}

func settingsAvailableModelAllowlist(
	modelConfig modelConfig,
	hasModelConfig bool,
	settings discoveredSettings,
) ([]string, bool) {
	var allowlist []string

	seen := make(map[string]struct{})

	if hasModelConfig && modelConfig.AvailableModels != nil {
		for _, model := range modelConfig.AvailableModels {
			if _, ok := seen[model]; ok {
				continue
			}

			allowlist = append(allowlist, model)
			seen[model] = struct{}{}
		}
	}

	if settings.HasAvailableModels {
		for _, model := range settings.AvailableModels {
			if _, ok := seen[model]; ok {
				continue
			}

			allowlist = append(allowlist, model)
			seen[model] = struct{}{}
		}
	}

	return allowlist, (hasModelConfig && modelConfig.AvailableModels != nil) || settings.HasAvailableModels
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}
