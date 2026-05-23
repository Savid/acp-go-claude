package claudeacp

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	metaAdditionalDirectoriesKey = "additionalDirectories"
	metaBareKey                  = "bare"
	metaModelKey                 = "model"
	metaOptionsKey               = "options"
	metaOutputFormatKey          = "outputFormat"
	metaOutputFormatSchemaKey    = "schema"
	metaOutputFormatTypeKey      = "type"
	metaPermissionModeKey        = "permissionMode"
	metaSystemPromptKey          = "systemPrompt"
)

// ClaudeOutputFormatJSONSchema selects Claude Code JSON Schema structured output.
const ClaudeOutputFormatJSONSchema = "json_schema"

// ClaudeOutputFormat configures Claude Code structured output for a session.
type ClaudeOutputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

// ClaudeOptions is the stable, supported Claude-specific subset accepted at
// _meta.claude.options. The JSON field names below are part of this
// package's wire contract; unsupported option keys are rejected.
type ClaudeOptions struct {
	// Bare launches Claude with --bare for this session.
	Bare bool `json:"bare,omitempty"`
	// Env adds environment variables for this Claude session.
	Env map[string]string `json:"env,omitempty"`
	// SystemPrompt overrides the default system prompt for this Claude session.
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// Model selects the initial Claude model for this session.
	Model string `json:"model,omitempty"`
	// PermissionMode selects the initial Claude permission mode for this session.
	PermissionMode string `json:"permissionMode,omitempty"`
	// AdditionalDirectories grants this Claude session access to extra directories.
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	// OutputFormat configures Claude Code structured output for this session.
	OutputFormat *ClaudeOutputFormat `json:"outputFormat,omitempty"`
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

	if options.SystemPrompt != "" {
		values[metaSystemPromptKey] = options.SystemPrompt
	}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.PermissionMode != "" {
		values[metaPermissionModeKey] = options.PermissionMode
	}

	if len(options.AdditionalDirectories) > 0 {
		values[metaAdditionalDirectoriesKey] = append([]string(nil), options.AdditionalDirectories...)
	}

	if options.OutputFormat != nil {
		values[metaOutputFormatKey] = map[string]any{
			metaOutputFormatTypeKey:   options.OutputFormat.Type,
			metaOutputFormatSchemaKey: cloneAnyMap(options.OutputFormat.Schema),
		}
	}

	return map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: values,
		},
	}
}

func claudeOptionsFromMeta(meta map[string]any) (ClaudeOptions, error) {
	options := ClaudeOptions{}
	claude, _ := meta[claudeMetaKey].(map[string]any)

	if rawOptions, ok := claude[metaOptionsKey]; ok {
		parsed, err := parseClaudeOptions(rawOptions)
		if err != nil {
			return ClaudeOptions{}, err
		}

		options = parsed
	}

	return options, nil
}

func sessionAdditionalDirectories(primary []string, options ClaudeOptions) []string {
	return mergeAdditionalDirectories(primary, options.AdditionalDirectories)
}

func outputFormatJSONSchema(outputFormat *ClaudeOutputFormat) map[string]any {
	if outputFormat == nil || outputFormat.Type != ClaudeOutputFormatJSONSchema {
		return nil
	}

	return cloneAnyMap(outputFormat.Schema)
}

func mergeAdditionalDirectories(primary []string, extra []string) []string {
	if len(extra) == 0 {
		return append([]string(nil), primary...)
	}

	merged := make([]string, 0, len(primary)+len(extra))
	merged = append(merged, primary...)
	merged = append(merged, extra...)

	return merged
}

func parseClaudeOptions(value any) (ClaudeOptions, error) {
	switch typed := value.(type) {
	case map[string]any:
		return parseClaudeOptionsMap(typed)
	default:
		return ClaudeOptions{}, fmt.Errorf("_meta.%s.%s must be an object", claudeMetaKey, metaOptionsKey)
	}
}

func parseClaudeOptionsMap(raw map[string]any) (ClaudeOptions, error) {
	options := ClaudeOptions{}

	for key, value := range raw {
		switch key {
		case metaBareKey:
			bare, ok := value.(bool)
			if !ok {
				return ClaudeOptions{}, fmt.Errorf("%s must be a boolean", metaOptionPath(key))
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
				return ClaudeOptions{}, fmt.Errorf("%s must be a string", metaOptionPath(key))
			}

			options.SystemPrompt = systemPrompt
		case metaModelKey:
			model, ok := value.(string)
			if !ok {
				return ClaudeOptions{}, fmt.Errorf("%s must be a string", metaOptionPath(key))
			}

			options.Model = model
		case metaPermissionModeKey:
			permissionMode, ok := value.(string)
			if !ok {
				return ClaudeOptions{}, fmt.Errorf("%s must be a string", metaOptionPath(key))
			}

			options.PermissionMode = permissionMode
		case metaAdditionalDirectoriesKey:
			additionalDirectories, err := stringSliceOption(value, metaOptionPath(key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.AdditionalDirectories = additionalDirectories
		case metaOutputFormatKey:
			outputFormat, err := outputFormatOption(value, metaOptionPath(key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.OutputFormat = outputFormat
		default:
			return ClaudeOptions{}, fmt.Errorf("%s is not supported", metaOptionPath(key))
		}
	}

	return validateClaudeOptions(options)
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

	if options.OutputFormat != nil {
		outputFormat, err := validateOutputFormat(*options.OutputFormat)
		if err != nil {
			return ClaudeOptions{}, err
		}

		options.OutputFormat = &outputFormat
	}

	return options, nil
}

func blockedClaudeEnvKey(key string) bool {
	upper := strings.ToUpper(key)
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
				return nil, fmt.Errorf("%s.%s must be a string", path, key)
			}

			result[key] = text
		}

		return result, nil
	default:
		return nil, fmt.Errorf("%s must be an object", path)
	}
}

func stringSliceOption(value any, path string) ([]string, error) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), nil
	case []any:
		result := make([]string, 0, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be a string", path, index)
			}

			result = append(result, text)
		}

		return result, nil
	default:
		return nil, fmt.Errorf("%s must be an array", path)
	}
}

func outputFormatOption(value any, path string) (*ClaudeOutputFormat, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", path)
	}

	format := ClaudeOutputFormat{}

	for key, item := range raw {
		switch key {
		case metaOutputFormatTypeKey:
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s.%s must be a string", path, key)
			}

			format.Type = text
		case metaOutputFormatSchemaKey:
			schema, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s.%s must be an object", path, key)
			}

			format.Schema = cloneAnyMap(schema)
		default:
			return nil, fmt.Errorf("%s.%s is not supported", path, key)
		}
	}

	return &format, nil
}

func validateOutputFormat(format ClaudeOutputFormat) (ClaudeOutputFormat, error) {
	if format.Type != ClaudeOutputFormatJSONSchema {
		return ClaudeOutputFormat{}, fmt.Errorf("%s.%s is not supported: %s",
			metaOptionPath(metaOutputFormatKey),
			metaOutputFormatTypeKey,
			format.Type,
		)
	}

	if len(format.Schema) == 0 {
		return ClaudeOutputFormat{}, fmt.Errorf("%s.%s must be a non-empty object",
			metaOptionPath(metaOutputFormatKey),
			metaOutputFormatSchemaKey,
		)
	}

	schema := cloneAnyMap(format.Schema)
	if _, err := json.Marshal(schema); err != nil {
		return ClaudeOutputFormat{}, fmt.Errorf("%s.%s must be JSON-serializable: %w",
			metaOptionPath(metaOutputFormatKey),
			metaOutputFormatSchemaKey,
			err,
		)
	}

	return ClaudeOutputFormat{
		Type:   format.Type,
		Schema: schema,
	}, nil
}
