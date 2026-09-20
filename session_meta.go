package claudeacp

import (
	"maps"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

const (
	metaOptionsKey        = "options"
	metaRawEventKey       = "rawEvent"
	metaModelKey          = "model"
	metaEnvKey            = "env"
	metaExtraPathDirsKey  = "extraPathDirs"
	metaOutputSchemaKey   = "outputSchema"
	metaEffortKey         = "effort"
	metaPermissionModeKey = "permissionMode"
	metaBareKey           = "bare"
	metaSystemPromptKey   = "systemPrompt"
	metaEnabledKey        = "enabled"

	// metaStructuredOutputKey names both the discovery object and the result
	// member native structured output lands on.
	metaStructuredOutputKey = "structuredOutput"
)

// ClaudeOptions is the per-session options struct carried at _meta.claude.options.
type ClaudeOptions struct {
	// Model selects the claude model for this session by native identifier.
	Model string `json:"model,omitempty"`
	// Env overlays the session's claude process environment.
	Env map[string]string `json:"env,omitempty"`
	// ExtraPathDirs are absolute directories prepended, in order, to the PATH
	// of this session's claude process.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
	// OutputSchema is the native structured-output JSON schema.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Effort is a reasoning-level value passed unchanged to claude.
	Effort string `json:"effort,omitempty"`
	// PermissionMode selects Claude's native permission policy.
	PermissionMode string `json:"permissionMode,omitempty"`
	// Bare requests Claude's native bare mode; an explicit false travels.
	Bare    bool `json:"bare,omitempty"`
	bareSet bool
	// SystemPrompt replaces the native system prompt.
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// ClaudeOption configures ClaudeOptions values.
type ClaudeOption func(*ClaudeOptions)

// NewClaudeOptions constructs ClaudeOptions from functional options.
func NewClaudeOptions(opts ...ClaudeOption) ClaudeOptions {
	options := ClaudeOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return options.clone()
}

// WithClaudeModel configures the session model by native identifier.
func WithClaudeModel(model string) ClaudeOption {
	return func(options *ClaudeOptions) { options.Model = model }
}

// WithClaudeEnv configures the session environment overlay.
func WithClaudeEnv(env map[string]string) ClaudeOption {
	cloned := maps.Clone(env)

	return func(options *ClaudeOptions) { options.Env = maps.Clone(cloned) }
}

// WithClaudeExtraPathDirs configures the directories prepended to the session PATH.
func WithClaudeExtraPathDirs(dirs ...string) ClaudeOption {
	cloned := slices.Clone(dirs)

	return func(options *ClaudeOptions) { options.ExtraPathDirs = slices.Clone(cloned) }
}

// WithClaudeEffort configures the reasoning level passed to claude.
func WithClaudeEffort(level string) ClaudeOption {
	return func(options *ClaudeOptions) { options.Effort = level }
}

// WithClaudePermissionMode selects the native permission policy.
func WithClaudePermissionMode(mode string) ClaudeOption {
	return func(options *ClaudeOptions) { options.PermissionMode = mode }
}

// WithClaudeBare requests Claude's native bare mode.
func WithClaudeBare(enabled bool) ClaudeOption {
	return func(options *ClaudeOptions) { options.Bare = enabled; options.bareSet = true }
}

// WithClaudeSystemPrompt replaces the native system prompt.
func WithClaudeSystemPrompt(text string) ClaudeOption {
	return func(options *ClaudeOptions) { options.SystemPrompt = text }
}

// WithClaudeOutputSchema configures native structured output.
func WithClaudeOutputSchema(schema map[string]any) ClaudeOption {
	cloned := wire.CloneMap(schema)

	return func(options *ClaudeOptions) { options.OutputSchema = wire.CloneMap(cloned) }
}

// Meta returns exactly {"claude": {"options": {...}}} with the non-zero fields.
func (options ClaudeOptions) Meta() map[string]any {
	values := map[string]any{}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.Env != nil {
		values[metaEnvKey] = maps.Clone(options.Env)
	}

	if options.ExtraPathDirs != nil {
		values[metaExtraPathDirsKey] = slices.Clone(options.ExtraPathDirs)
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = wire.CloneMap(options.OutputSchema)
	}

	if options.Effort != "" {
		values[metaEffortKey] = options.Effort
	}

	if options.PermissionMode != "" {
		values[metaPermissionModeKey] = options.PermissionMode
	}

	if options.Bare || options.bareSet {
		values[metaBareKey] = options.Bare
	}

	if options.SystemPrompt != "" {
		values[metaSystemPromptKey] = options.SystemPrompt
	}

	return map[string]any{vendor: map[string]any{metaOptionsKey: values}}
}

func (options ClaudeOptions) clone() ClaudeOptions {
	cloned := options
	cloned.Env = maps.Clone(options.Env)
	cloned.ExtraPathDirs = slices.Clone(options.ExtraPathDirs)
	cloned.OutputSchema = wire.CloneMap(options.OutputSchema)

	return cloned
}

// ValidateClaudeSessionMeta runs the owned-namespace parsing of a session
// lifecycle request's _meta without an Agent and returns the same refusal.
func ValidateClaudeSessionMeta(meta map[string]any) error {
	_, err := parseSessionMeta(meta)
	if err != nil {
		return err
	}

	return nil
}

// sessionMeta is what one session lifecycle request's _meta.claude carried.
type sessionMeta struct {
	options   ClaudeOptions
	rawEvents bool
	// present records which carrier fields the request named, so a load or
	// resume inherits the stored value only for fields it left out.
	presentEnv           bool
	presentExtraPathDirs bool
}

// parseSessionMeta validates the owned _meta.claude namespace of one session
// lifecycle request. Unknown own-namespace keys fail closed; foreign
// namespaces are ignored; the lifecycle literal is refused by name.
func parseSessionMeta(meta map[string]any) (sessionMeta, *acp.RequestError) {
	if refusal := lifecycle.RejectKey(meta); refusal != nil {
		return sessionMeta{}, wire.ParamRefusal(refusal)
	}

	raw, exists := meta[vendor]
	if !exists {
		return sessionMeta{}, nil
	}

	vendorMeta, ok := raw.(map[string]any)
	if !ok {
		return sessionMeta{}, wire.Unsupported("_meta." + vendor)
	}

	parsed := sessionMeta{}

	for key := range vendorMeta {
		switch key {
		case metaOptionsKey, metaRawEventKey:
		default:
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + key)
		}
	}

	if rawEvent, ok := vendorMeta[metaRawEventKey]; ok {
		values, ok := rawEvent.(map[string]any)
		if !ok {
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey)
		}

		for key, item := range values {
			enabled, ok := item.(bool)
			if key != metaEnabledKey || !ok {
				return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey + "." + key)
			}

			parsed.rawEvents = enabled
		}
	}

	rawOptions, hasOptions := vendorMeta[metaOptionsKey]
	if !hasOptions {
		return parsed, nil
	}

	values, isObject := rawOptions.(map[string]any)
	if !isObject {
		return sessionMeta{}, wire.Unsupported(wire.MetaOptionPath(vendor, ""))
	}

	options, err := parseClaudeOptions(values)
	if err != nil {
		return sessionMeta{}, err
	}

	parsed.options = options
	_, parsed.presentEnv = values[metaEnvKey]
	_, parsed.presentExtraPathDirs = values[metaExtraPathDirsKey]

	return parsed, nil
}

func parseClaudeOptions(values map[string]any) (ClaudeOptions, *acp.RequestError) {
	options := ClaudeOptions{}

	for key, item := range values {
		switch key {
		case metaModelKey:
			model, ok := item.(string)
			if !ok || model == "" {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.Model = model
		case metaEnvKey:
			env, err := wire.StringMapOption(item, wire.MetaOptionPath(vendor, key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.Env = env
		case metaExtraPathDirsKey:
			dirs, err := wire.StringSliceOption(item, wire.MetaOptionPath(vendor, key))
			if err != nil {
				return ClaudeOptions{}, err
			}

			options.ExtraPathDirs = dirs
		case metaOutputSchemaKey:
			schema, ok := item.(map[string]any)
			if !ok {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.OutputSchema = wire.CloneMap(schema)
		case metaEffortKey:
			level, ok := item.(string)
			if !ok || level == "" {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.Effort = level
		case metaPermissionModeKey:
			permission, ok := item.(string)
			if !ok {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.PermissionMode = permission
		case metaSystemPromptKey:
			text, ok := item.(string)
			if !ok {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.SystemPrompt = text
		case metaBareKey:
			enabled, ok := item.(bool)
			if !ok {
				return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
			}

			options.Bare = enabled
			options.bareSet = true
		default:
			return ClaudeOptions{}, wire.Unsupported(wire.MetaOptionPath(vendor, key))
		}
	}

	return options, validateClaudeOptions(options)
}

func validateClaudeOptions(options ClaudeOptions) *acp.RequestError {
	if options.OutputSchema != nil && len(options.OutputSchema) == 0 {
		return wire.Unsupported(wire.MetaOptionPath(vendor, metaOutputSchemaKey))
	}

	if options.PermissionMode != "" && !slices.Contains([]string{nativeDefault, "manual", permissionModePlan, "acceptEdits", "bypassPermissions", "auto", "dontAsk"}, options.PermissionMode) {
		return wire.Unsupported(wire.MetaOptionPath(vendor, metaPermissionModeKey))
	}

	return wire.ValidateSessionEnvironment(options.Env, options.ExtraPathDirs, wire.MetaOptionPath(vendor, ""))
}
