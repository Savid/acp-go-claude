package claude

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	modelCatalogEndpoint  = "https://api.anthropic.com/v1/models"
	anthropicModelPrefix  = "claude-"
	anthropicModelDefault = "default"
	anthropicModelSonnet  = "sonnet"
	modelDiscoveryTimeout = 3 * time.Second

	// nativeModelDisabledKey is the native model field naming an explicit refusal.
	nativeModelDisabledKey = "disabled"

	// nativeTokenSourceNone is Claude's own word for holding no bearer.
	nativeTokenSourceNone = "none"
)

// anthropicModelAliases are the family names Claude's own menu uses for
// Anthropic models. `default` joins them: with nothing concrete resolved behind
// it, it selects whichever Anthropic model the harness would choose.
var anthropicModelAliases = []string{anthropicModelDefault, anthropicModelSonnet, "opus", "haiku", "fable"}

// ModelCatalogReader supplies provider model facts for one effective credential.
// The owning Agent shares the reader across sessions; native restrictions are
// reconciled separately for every process.
type ModelCatalogReader interface {
	List(context.Context, ModelCatalogAccess) ([]APIModel, error)
}

// DiscoverModels reads this process's model configuration.
//
// Claude's own menu names Anthropic's models. Every provider Claude reports
// serves those models under its own account, so the menu stands as given unless
// this process cannot dispatch those names: see routedNativeModels for the
// conditions. Direct Anthropic credentials additionally enrich the native rows
// from the provider catalog; unauthenticated processes never consult or
// populate the provider cache.
func (c *Client) DiscoverModels(
	ctx context.Context,
	catalog ModelCatalogReader,
	direct bool,
) ([]AvailableModelInfo, *SettingsSnapshot, error) {
	settings, err := c.GetSettings(ctx)
	if err != nil {
		return c.routedNativeModels(ctx, nil), nil, err
	}

	access, eligible := c.modelCatalogAccess(settings)
	if !direct || catalog == nil || !eligible {
		return c.routedNativeModels(ctx, settings), settings, nil
	}

	return c.discoverDirectModels(ctx, catalog, settings, access)
}

// discoverDirectModels merges Anthropic's Models API facts into the native
// list. Eligibility means an unambiguous credential for Anthropic's own
// endpoint, so such a process withholds nothing and every native row stands.
func (c *Client) discoverDirectModels(
	ctx context.Context,
	catalog ModelCatalogReader,
	settings *SettingsSnapshot,
	access ModelCatalogAccess,
) ([]AvailableModelInfo, *SettingsSnapshot, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, modelDiscoveryTimeout)
	defer cancel()

	// initialize is a startup snapshot and excludes disabled rows. A fresh
	// native list carries explicit refusals that provider discovery must honor.
	native, err := c.listModels(discoverCtx)
	if err != nil {
		return c.InitializeInfo().Models, settings, err
	}

	models, readErr := catalog.List(discoverCtx, access)
	if readErr != nil {
		return native, settings, readErr
	}

	// Settings may change while a remote read is in flight. Never attach a
	// catalog acquired for one credential to a differently configured process.
	currentSettings, err := c.GetSettings(discoverCtx)
	if err != nil {
		return native, settings, err
	}

	current, eligible := c.modelCatalogAccess(currentSettings)
	if !eligible || current != access {
		return c.routedNativeModels(discoverCtx, currentSettings), currentSettings, nil
	}

	overrides, _ := currentSettings.Effective["modelOverrides"].(map[string]any)

	return mergeModelCatalog(models, native, overrides), currentSettings, nil
}

// routedNativeModels is the native list this process may publish. Claude's own
// menu names Anthropic's models, and two conditions make them unusable: a
// process reaching the Anthropic API at another endpoint cannot dispatch them,
// and a process Claude reports as holding no credential cannot dispatch
// anything. Either withdraws those names and leaves what the endpoint
// enumerated for itself. Rows Claude explicitly refused are kept whatever the
// route: an allowlist or an explicit selection must not reintroduce a refused
// model under another alias.
func (c *Client) routedNativeModels(ctx context.Context, settings *SettingsSnapshot) []AvailableModelInfo {
	if !c.WithholdsAnthropicNames(settings) {
		return c.InitializeInfo().Models
	}

	// The startup snapshot is neither complete nor current for a withholding
	// process: it omits the rows Claude refused, and Claude's own gateway
	// discovery lands after initialize has already answered.
	native, err := c.listModels(ctx)
	if err != nil {
		native = c.InitializeInfo().Models
	}

	kept := make([]AvailableModelInfo, 0, len(native))
	for _, model := range native {
		if model.Disabled || !AnthropicModelIdentity(model) {
			kept = append(kept, model)
		}
	}

	return kept
}

// WithholdsAnthropicNames reports whether this process publishes no Anthropic
// model names: it is routed to another endpoint, or Claude reports that no
// credential signs its requests.
func (c *Client) WithholdsAnthropicNames(settings *SettingsSnapshot) bool {
	return c.gatewayRouted(settings) || c.credentialAbsent()
}

// gatewayRouted reports whether this process talks the Anthropic API to an
// endpoint other than Anthropic's own. Claude names the provider it resolved,
// and every provider it names but `firstParty` serves Anthropic's models
// itself, so only a first-party process pointed at another host has a menu it
// cannot dispatch. Claude does not report the host, so the base URL places a
// first-party process, and the route test is Claude's own: a spelling Claude
// treats as its endpoint keeps the menu Claude is serving. A provider Claude
// will not name, and a launch environment this adapter cannot read, leave the
// route unestablished rather than first-party, and an unestablished route is no
// evidence that these names dispatch.
func (c *Client) gatewayRouted(settings *SettingsSnapshot) bool {
	provider := c.InitializeInfo().APIProvider
	if provider == "" {
		return true
	}

	if provider != apiProviderFirstParty {
		return false
	}

	env, ok := c.launchEnvironment()
	if !ok {
		return true
	}

	return !firstPartyRoute(effectiveRouteEnvironment(settings, env))
}

// credentialAbsent reports whether Claude says this process holds no
// credential. Claude names its own: `tokenSource` is the variable a bearer came
// from and reads `none` when there is no bearer, `apiKeySource` names an API
// key when one is configured, and a signed-in subscription reports neither
// field. Absence of both is silence rather than a report — a Bedrock, Vertex or
// Foundry process resolves its credential through that cloud's own chain and
// Claude names nothing here — so only an explicit `none` standing alone
// establishes that nothing signs this process's requests.
func (c *Client) credentialAbsent() bool {
	info := c.InitializeInfo()

	return info.TokenSource == nativeTokenSourceNone && info.APIKeySource == ""
}

// Settings env overrides the launch environment for both route controls.
func effectiveRouteEnvironment(settings *SettingsSnapshot, env map[string]string) map[string]string {
	if settings == nil || settings.Effective == nil {
		return env
	}

	values, _ := settings.Effective["env"].(map[string]any)

	resolved := maps.Clone(env)
	if resolved == nil {
		resolved = make(map[string]string)
	}

	for _, key := range []string{directAPIBaseURLEnv, assumeFirstPartyEnv} {
		if value, ok := values[key].(string); ok {
			resolved[EnvironmentKey(key)] = value
		}
	}

	return resolved
}

// AnthropicModelIdentity reports whether a model row names an Anthropic model:
// a `claude-` prefixed id, or one of Claude's family aliases with no concrete
// target behind it. A row whose target is some other id was contributed by a
// provider Claude was pointed at, and belongs to that provider.
func AnthropicModelIdentity(model AvailableModelInfo) bool {
	if resolved := NormalizeModelIdentity(model.ResolvedModel); resolved != "" {
		return strings.HasPrefix(resolved, anthropicModelPrefix)
	}

	value := NormalizeModelIdentity(model.Value)

	return strings.HasPrefix(value, anthropicModelPrefix) || slices.Contains(anthropicModelAliases, value)
}

// NormalizeModelIdentity reduces a model name to the identity two spellings of
// the same model share: case, surrounding space, and the `[1m]` context-window
// suffix all name the same model to Claude.
func NormalizeModelIdentity(id string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(id)), "[1m]")
}

func (c *Client) listModels(ctx context.Context) ([]AvailableModelInfo, error) {
	controller := c.activeController()
	if controller == nil {
		return nil, ErrClientNotStarted
	}

	response, err := controller.SendRequest(ctx, "list_models", nil, modelDiscoveryTimeout)
	if err != nil {
		return nil, errors.New("native model discovery failed")
	}

	payload, _ := response.Response[keyResponse].(map[string]any)
	if _, ok := payload["models"].([]any); !ok {
		return nil, errors.New("native model discovery response invalid")
	}

	return parseAvailableModels(payload["models"]), nil
}

func (c *Client) modelCatalogAccess(settings *SettingsSnapshot) (ModelCatalogAccess, bool) {
	env, ok := c.launchEnvironment()
	if !ok || settings == nil || settings.Effective == nil ||
		!directAPISettingsMatch(settings.Effective, env) {
		return ModelCatalogAccess{}, false
	}

	access, eligible := resolveModelCatalogAccess(env)
	if access.OAuth {
		// Native bare mode ignores OAuth, including an explicit environment
		// token. Its equivalent SIMPLE switch uses Claude's truthy spellings.
		if c.options.Bare || claudeSwitchEnabled(env[EnvironmentKey("CLAUDE_CODE_SIMPLE")]) {
			return ModelCatalogAccess{}, false
		}
	}

	return access, eligible
}

func (c *Client) launchEnvironment() (map[string]string, bool) {
	source, ok := c.transport.(interface{ LaunchEnvironment() map[string]string })
	if !ok || c.isClosed() {
		return nil, false
	}

	return source.LaunchEnvironment(), true
}

func resolveModelCatalogAccess(env map[string]string) (ModelCatalogAccess, bool) {
	if directAPIHasRouteOverride(env) || strings.TrimSpace(env[EnvironmentKey(directAPIInternalBaseURLEnv)]) != "" {
		return ModelCatalogAccess{}, false
	}

	if !anthropicBaseURL(env) {
		return ModelCatalogAccess{}, false
	}

	token := env[EnvironmentKey(directAPIOAuthTokenEnv)]

	key := env[EnvironmentKey(directAPIKeyEnv)]
	if (token == "") == (key == "") {
		return ModelCatalogAccess{}, false
	}

	credential := key
	if token != "" {
		credential = token
	}

	// Auth is forwarded byte-for-byte. Trimming a malformed credential would
	// make this read authenticate differently from the native process.
	for _, character := range credential {
		if character < '!' || character > '~' {
			return ModelCatalogAccess{}, false
		}
	}

	return ModelCatalogAccess{Endpoint: modelCatalogEndpoint, Credential: credential, OAuth: token != ""}, true
}

func mergeModelCatalog(api []APIModel, native []AvailableModelInfo, overrides map[string]any) []AvailableModelInfo {
	byID := make(map[string]APIModel, len(api))
	for _, model := range api {
		byID[model.ID] = model
	}

	result := CloneAvailableModels(native)
	seen := make(map[string]struct{}, len(native)+len(api))
	nativeByID := make(map[string]AvailableModelInfo, len(native))

	for i := range result {
		model := &result[i]
		if model.EffortUnsupported {
			model.SupportedEffortLevels = nil
		}

		seen[model.Value] = struct{}{}

		id := model.ResolvedModel
		if id == "" {
			id = model.Value
		}

		// Prefer a concrete native row to an alias for metadata reconciliation.
		if _, exists := nativeByID[id]; !exists || model.Value == id {
			nativeByID[id] = *model
		}

		if provider, ok := byID[id]; ok && (model.ResolvedModel != "" || !nativeModelOverride(model.Value, overrides)) {
			model.ContextWindow = provider.ContextWindow

			model.MaxOutputTokens = provider.MaxOutputTokens
			if len(model.SupportedEffortLevels) == 0 && !model.EffortUnsupported {
				model.SupportedEffortLevels = slices.Clone(provider.SupportedEffortLevels)
			}

			if model.DisplayName == "" {
				model.DisplayName = provider.DisplayName
			}
		}
	}

	for _, provider := range api {
		if _, exists := seen[provider.ID]; exists {
			continue
		}

		model := AvailableModelInfo{
			Value: provider.ID, ResolvedModel: provider.ID, DisplayName: provider.DisplayName,
			ContextWindow: provider.ContextWindow, MaxOutputTokens: provider.MaxOutputTokens,
			SupportedEffortLevels: slices.Clone(provider.SupportedEffortLevels),
		}
		if nativeModelOverride(provider.ID, overrides) {
			// Claude applies these mappings only to IDs its installed version
			// recognizes. An API-only row cannot prove whether the mapping takes
			// effect. Keep its dispatch value and omit unproven identity/facts;
			// an exact native row above already has its resolved metadata.
			result = append(result, AvailableModelInfo{Value: provider.ID, DisplayName: provider.ID})
			seen[provider.ID] = struct{}{}

			continue
		}

		if info, ok := nativeByID[provider.ID]; ok {
			model.SupportsAutoMode = info.SupportsAutoMode

			model.EffortUnsupported = info.EffortUnsupported
			if info.EffortUnsupported {
				model.SupportedEffortLevels = nil
			} else if len(info.SupportedEffortLevels) > 0 {
				model.SupportedEffortLevels = slices.Clone(info.SupportedEffortLevels)
			}
		}

		result = append(result, model)
		seen[provider.ID] = struct{}{}
	}

	// Keep disabled records internally so allowlist application and an explicit
	// selection cannot reintroduce a native-refused model under another alias.
	for i := range result {
		if ModelDisabled(result[i].Value, native) || ModelDisabled(result[i].ResolvedModel, native) {
			result[i].Disabled = true
		}
	}

	return result
}

func nativeModelOverride(value string, overrides map[string]any) bool {
	target, _ := overrides[value].(string)

	return target != "" && target != value
}

// ModelDisabled reports an explicit native denial, matched by exact identity.
// Absence from a catalog never implies denial or lack of provider entitlement.
func ModelDisabled(value string, models []AvailableModelInfo) bool {
	if value == "" {
		return false
	}

	for _, model := range models {
		if model.Disabled && (value == model.Value || value == model.ResolvedModel) {
			return true
		}
	}

	return false
}

// CloneAvailableModels gives each session ownership of its metadata slices.
func CloneAvailableModels(models []AvailableModelInfo) []AvailableModelInfo {
	cloned := slices.Clone(models)
	for i := range cloned {
		cloned[i].SupportedEffortLevels = slices.Clone(cloned[i].SupportedEffortLevels)
	}

	return cloned
}
