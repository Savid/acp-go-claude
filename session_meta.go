package claudeacp

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	metaAdditionalDirectoriesKey = "additionalDirectories"
	metaBareKey                  = "bare"
	metaExtraPathDirsKey         = "extraPathDirs"
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
	// ExtraPathDirs are absolute directories prepended, in order, to the PATH of
	// this session's Claude process. They precede every inherited entry, so an
	// executable placed here shadows the one PATH would otherwise resolve. Raw
	// PATH stays rejected in Env: this is the only supported way to extend it.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
	// OutputSchema configures Claude Code JSON Schema structured output.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// SystemPrompt overrides the default system prompt for this Claude session.
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// PermissionMode selects the initial Claude permission mode for this session.
	PermissionMode string `json:"permissionMode,omitempty"`
	// ProviderAuth carries host-owned credentials into one session launch.
	ProviderAuth map[string]ProviderAuthBinding `json:"providerAuth,omitempty"`
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

	if len(options.ExtraPathDirs) > 0 {
		values[metaExtraPathDirsKey] = slices.Clone(options.ExtraPathDirs)
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

	if len(options.ProviderAuth) > 0 {
		values["providerAuth"] = cloneProviderAuthBindings(options.ProviderAuth)
	}

	return map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: values,
		},
	}
}

func claudeOptionsFromMeta(meta map[string]any) (ClaudeOptions, error) {
	return claudeOptionsFromMetaWithProviderAuth(meta, false)
}

func claudeOptionsFromMetaWithProviderAuth(meta map[string]any, providerAuthEnabled bool) (ClaudeOptions, error) {
	options := ClaudeOptions{}

	namespace, ok := meta[claudeMetaKey].(map[string]any)
	if !ok {
		if _, exists := meta[claudeMetaKey]; exists {
			return ClaudeOptions{}, unsupportedField("_meta." + claudeMetaKey)
		}
	}

	if err := validateClaudeLifecycleMeta(namespace); err != nil {
		return ClaudeOptions{}, err
	}

	if rawOptions, ok := namespace[metaOptionsKey]; ok {
		parsed, err := parseClaudeOptions(rawOptions, providerAuthEnabled)
		if err != nil {
			return ClaudeOptions{}, err
		}

		options = parsed
	}

	return options, nil
}

func validateClaudeLifecycleMeta(namespace map[string]any) error {
	if namespace == nil {
		return nil
	}

	for key := range namespace {
		switch key {
		case metaOptionsKey, metaRawEventKey:
		default:
			return unsupportedField("_meta." + claudeMetaKey + "." + key)
		}
	}

	if rawEvent, ok := namespace[metaRawEventKey]; ok {
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

func parseClaudeOptions(value any, providerAuthEnabled bool) (ClaudeOptions, error) {
	switch typed := value.(type) {
	case map[string]any:
		return parseClaudeOptionsMap(typed, providerAuthEnabled)
	default:
		return ClaudeOptions{}, unsupportedField("_meta." + claudeMetaKey + "." + metaOptionsKey)
	}
}

func parseClaudeOptionsMap(raw map[string]any, providerAuthEnabled bool) (ClaudeOptions, error) {
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
		case metaExtraPathDirsKey:
			dirs, err := stringSliceOption(value, metaOptionPath(key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.ExtraPathDirs = dirs
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
			// An empty object is refused here rather than treated as "no
			// schema": a host that asked for structured output and got an
			// ordinary turn back has no way to tell the two apart.
			schema, ok := value.(map[string]any)
			if !ok || len(schema) == 0 {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			options.OutputSchema = cloneAnyMap(schema)
		case "providerAuth":
			if !providerAuthEnabled {
				return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
			}

			bindings, err := providerAuthBindingsOption(value)
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.ProviderAuth = bindings
		default:
			return ClaudeOptions{}, unsupportedField(metaOptionPath(key))
		}
	}

	return validateClaudeOptions(options)
}

func providerAuthBindingsOption(value any) (map[string]ProviderAuthBinding, error) {
	if typed, ok := value.(map[string]ProviderAuthBinding); ok {
		if len(typed) != 1 || !validProviderCredential(typed[authProviderID].Credential) {
			return nil, unsupportedField(providerAuthOptionPath)
		}

		binding := typed[authProviderID]
		if !authValidConnectionID(binding.ConnectionID) || binding.Revision <= 0 || binding.BindingGeneration <= 0 {
			return nil, unsupportedField(providerAuthOptionPath + "." + authProviderID)
		}

		return cloneProviderAuthBindings(typed), nil
	}

	raw, ok := value.(map[string]any)
	if !ok || len(raw) != 1 {
		return nil, unsupportedField(providerAuthOptionPath)
	}

	wire, ok := raw[authProviderID]
	if !ok {
		return nil, unsupportedField(providerAuthOptionPath)
	}

	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, unsupportedField(providerAuthOptionPath + "." + authProviderID)
	}

	var binding ProviderAuthBinding

	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()

	if decoder.Decode(&binding) != nil || !authValidConnectionID(binding.ConnectionID) ||
		binding.Revision <= 0 || binding.BindingGeneration <= 0 ||
		!validProviderCredential(binding.Credential) {
		return nil, unsupportedField(providerAuthOptionPath + "." + authProviderID)
	}

	return map[string]ProviderAuthBinding{authProviderID: binding}, nil
}

func cloneProviderAuthBindings(source map[string]ProviderAuthBinding) map[string]ProviderAuthBinding {
	if len(source) == 0 {
		return nil
	}

	cloned := make(map[string]ProviderAuthBinding, len(source))
	for providerID, binding := range source {
		if binding.Credential.API != nil {
			api := *binding.Credential.API
			api.Metadata = maps.Clone(api.Metadata)
			binding.Credential.API = &api
		}

		cloned[providerID] = binding
	}

	return cloned
}

func unsupportedField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: validationUnsupported,
		jsonFieldField: path,
	})
}

func validateClaudeOptions(options ClaudeOptions) (ClaudeOptions, error) {
	if strings.TrimSpace(options.PermissionMode) != "" && !validClaudePermissionMode(options.PermissionMode) {
		return ClaudeOptions{}, unsupportedField(metaOptionPath(metaPermissionModeKey))
	}

	for key := range options.Env {
		if !validSettingsEnvName(key) || blockedClaudeEnvKey(key) {
			return ClaudeOptions{}, unsupportedField(metaOptionPath(settingsFieldEnv) + "." + key)
		}
	}

	for index, dir := range options.ExtraPathDirs {
		if !filepath.IsAbs(dir) || strings.ContainsRune(dir, os.PathListSeparator) {
			return ClaudeOptions{}, unsupportedField(
				metaOptionPath(metaExtraPathDirsKey) + "[" + strconv.Itoa(index) + "]",
			)
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

// blockedClaudeEnvKey reports whether a host-supplied session env key names a
// variable the adapter refuses to forward. Every name here is one the native
// process, its loader, or its shell reads under an exact platform spelling, so
// the comparison goes through the platform seam: on Unix `path` and `env` are
// variables of the host's own, and refusing them would break a legitimate
// session over a name nothing dangerous ever reads.
func blockedClaudeEnvKey(key string) bool {
	if privateAdapterEnvName(key) || managedClaudeRootEnvKey(key) {
		return true
	}

	name := claude.EnvironmentKey(key)

	switch name {
	case "PATH", "NODE_OPTIONS", "BASH_ENV", "ENV", "CLAUDECODE": //nolint:goconst // Protocol allowlist is clearer with literal names.
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
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

func stringSliceOption(value any, path string) ([]string, error) {
	switch typed := value.(type) {
	case []string:
		return slices.Clone(typed), nil
	case []any:
		result := make([]string, 0, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, unsupportedField(path + "[" + strconv.Itoa(index) + "]")
			}

			result = append(result, text)
		}

		return result, nil
	default:
		return nil, unsupportedField(path)
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	return maps.Clone(values)
}

func validateOutputSchema(schema map[string]any) (map[string]any, error) {
	cloned := cloneAnyMap(schema)
	if _, err := json.Marshal(cloned); err != nil {
		return nil, unsupportedField(metaOptionPath(metaOutputSchemaKey))
	}

	return cloned, nil
}
