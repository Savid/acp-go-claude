package claudeacp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	metaAdditionalDirectoriesKey = "additionalDirectories"
	metaBareKey                  = "bare"
	metaModelKey                 = "model"
	metaOptionsKey               = "options"
	metaOutputSchemaKey          = "outputSchema"
	metaPermissionModeKey        = "permissionMode"
	metaRawEventKey              = "rawEvent"
	metaRawEventEnabledKey       = "enabled"
	metaSystemPromptKey          = "systemPrompt"
)

// ClaudeOptions is the stable, supported Claude-specific subset accepted at
// _meta.claude.options. The JSON field names below are part of this
// package's wire contract; unsupported option keys are rejected.
type ClaudeOptions struct {
	// Model selects the initial Claude model for this session.
	Model string `json:"model,omitempty"`
	// Bare launches Claude with --bare for this session.
	Bare bool `json:"bare,omitempty"`
	// Env adds environment variables for this Claude session.
	Env map[string]string `json:"env,omitempty"`
	// OutputSchema configures Claude Code JSON Schema structured output.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// SystemPrompt overrides the default system prompt for this Claude session.
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// PermissionMode selects the initial Claude permission mode for this session.
	PermissionMode string `json:"permissionMode,omitempty"`
}

// Meta returns an ACP _meta object for the supported Claude-specific options.
func (options ClaudeOptions) Meta() map[string]any {
	values := map[string]any{}
	if options.Bare {
		values[metaBareKey] = true
	}

	if len(options.Env) > 0 {
		values[settingsFieldEnv] = cloneStringMap(options.Env)
	}

	if len(options.OutputSchema) > 0 {
		values[metaOutputSchemaKey] = cloneAnyMap(options.OutputSchema)
	}

	if options.SystemPrompt != "" {
		values[metaSystemPromptKey] = options.SystemPrompt
	}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.PermissionMode != "" {
		values[metaPermissionModeKey] = options.PermissionMode
	}

	return map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: values,
		},
	}
}

func claudeOptionsFromMeta(meta map[string]any) (ClaudeOptions, error) {
	options := ClaudeOptions{}

	claude, ok := meta[claudeMetaKey].(map[string]any)
	if !ok {
		if _, exists := meta[claudeMetaKey]; exists {
			return ClaudeOptions{}, unsupportedField("_meta." + claudeMetaKey)
		}
	}

	if err := validateClaudeLifecycleMeta(claude); err != nil {
		return ClaudeOptions{}, err
	}

	if rawOptions, ok := claude[metaOptionsKey]; ok {
		parsed, err := parseClaudeOptions(rawOptions)
		if err != nil {
			return ClaudeOptions{}, err
		}

		options = parsed
	}

	return options, nil
}

func validateClaudeLifecycleMeta(claude map[string]any) error {
	if claude == nil {
		return nil
	}

	for key := range claude {
		switch key {
		case metaOptionsKey, metaRawEventKey:
		default:
			return unsupportedField("_meta." + claudeMetaKey + "." + key)
		}
	}

	if rawEvent, ok := claude[metaRawEventKey]; ok {
		if err := validateRawEventMeta(rawEvent); err != nil {
			return err
		}
	}

	return nil
}

func validateRawEventMeta(value any) error {
	raw, ok := value.(map[string]any)
	if !ok {
		return unsupportedField("_meta." + claudeMetaKey + "." + metaRawEventKey)
	}

	for key, item := range raw {
		switch key {
		case metaRawEventEnabledKey:
			if _, ok := item.(bool); !ok {
				return unsupportedField("_meta." + claudeMetaKey + "." + metaRawEventKey + "." + key)
			}
		default:
			return unsupportedField("_meta." + claudeMetaKey + "." + metaRawEventKey + "." + key)
		}
	}

	return nil
}

func sessionAdditionalDirectories(primary []string) []string {
	return append([]string(nil), primary...)
}

func outputSchemaJSONSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return nil
	}

	return cloneAnyMap(schema)
}

func parseClaudeOptions(value any) (ClaudeOptions, error) {
	switch typed := value.(type) {
	case map[string]any:
		return parseClaudeOptionsMap(typed)
	default:
		return ClaudeOptions{}, unsupportedField("_meta." + claudeMetaKey + "." + metaOptionsKey)
	}
}

func parseClaudeOptionsMap(raw map[string]any) (ClaudeOptions, error) {
	options := ClaudeOptions{}

	for key, value := range raw {
		switch key {
		case metaBareKey:
			bare, ok := value.(bool)
			if !ok {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.Bare = bare
		case settingsFieldEnv:
			env, err := stringMapOption(value, metaOptionPath(key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.Env = env
		case metaSystemPromptKey:
			systemPrompt, ok := value.(string)
			if !ok {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.SystemPrompt = systemPrompt
		case metaModelKey:
			model, ok := value.(string)
			if !ok {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.Model = model
		case metaPermissionModeKey:
			permissionMode, ok := value.(string)
			if !ok {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.PermissionMode = permissionMode
		case metaOutputSchemaKey:
			schema, ok := value.(map[string]any)
			if !ok {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.OutputSchema = cloneAnyMap(schema)
		default:
			return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
		}
	}

	return validateClaudeOptions(options)
}

func unsupportedField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: validationUnsupported,
		jsonFieldField: path,
	})
}

func validateClaudeOptions(options ClaudeOptions) (ClaudeOptions, error) {
	if strings.TrimSpace(options.PermissionMode) != "" && !validClaudePermissionMode(options.PermissionMode) {
		return ClaudeOptions{}, fmt.Errorf("%s is not supported: %s", metaOptionPath(metaPermissionModeKey), options.PermissionMode)
	}

	for key := range options.Env {
		if !validSettingsEnvName(key) {
			return ClaudeOptions{}, fmt.Errorf("%s.%s is not a valid environment variable name", metaOptionPath(settingsFieldEnv), key)
		}

		if blockedClaudeEnvKey(key) {
			return ClaudeOptions{}, fmt.Errorf("%s.%s is not allowed", metaOptionPath(settingsFieldEnv), key)
		}
	}

	if len(options.OutputSchema) > 0 {
		outputSchema, err := validateOutputSchema(options.OutputSchema)
		if err != nil {
			return ClaudeOptions{}, err
		}

		options.OutputSchema = outputSchema
	}

	return options, nil
}

func blockedClaudeEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, privateAdapterEnvPrefix) {
		return true
	}

	switch upper {
	case "PATH", "NODE_OPTIONS", "BASH_ENV", "ENV", "CLAUDECODE":
		return true
	default:
		return strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_")
	}
}

func validClaudePermissionMode(mode string) bool {
	switch mode {
	case string(modeDefault), permissionModeAcceptEdits, permissionModeBypassPermissions, string(modePlan), permissionModeDontAsk, string(modeAuto):
		return true
	default:
		return false
	}
}

func metaOptionPath(key string) string {
	return "_meta." + claudeMetaKey + "." + metaOptionsKey + "." + key
}

func stringMapOption(value any, path string) (map[string]string, error) {
	switch typed := value.(type) {
	case map[string]string:
		return cloneStringMap(typed), nil
	case map[string]any:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, unsupportedField(path + "." + key)
			}

			result[key] = text
		}

		return result, nil
	default:
		return nil, unsupportedField(path)
	}
}

func validateOutputSchema(schema map[string]any) (map[string]any, error) {
	if len(schema) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty object", metaOptionPath(metaOutputSchemaKey))
	}

	cloned := cloneAnyMap(schema)
	if _, err := json.Marshal(cloned); err != nil {
		return nil, fmt.Errorf("%s must be JSON-serializable: %w", metaOptionPath(metaOutputSchemaKey), err)
	}

	return cloned, nil
}
