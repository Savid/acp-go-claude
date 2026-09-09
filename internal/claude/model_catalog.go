package claude

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	modelCatalogEndpoint = "https://api.anthropic.com/v1/models"
	modelCatalogEnvYes   = "yes"
)

// ModelCatalogReader supplies provider model facts for one effective credential.
// The owning Agent shares the reader across sessions; native restrictions are
// reconciled separately for every process.
type ModelCatalogReader interface {
	List(context.Context, ModelCatalogAccess) ([]APIModel, error)
}

// DiscoverModels reads this process's model configuration. Direct Anthropic
// credentials use the provider catalog; native choices remain the operational
// fallback when provider discovery is unavailable. Unauthenticated processes
// never consult or populate the provider cache.
func (c *Client) DiscoverModels(
	ctx context.Context,
	catalog ModelCatalogReader,
	direct bool,
) ([]AvailableModelInfo, *SettingsSnapshot, error) {
	native := c.InitializeInfo().Models

	settings, err := c.GetSettings(ctx)
	if err != nil {
		return native, nil, err
	}

	access, eligible := c.modelCatalogAccess(settings)
	if !direct || catalog == nil || !eligible {
		return native, settings, nil
	}

	discoverCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// initialize is a startup snapshot and excludes disabled rows. A fresh
	// native list carries explicit refusals that provider discovery must honor.
	native, err = c.listModels(discoverCtx)
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
		return native, currentSettings, nil
	}

	overrides, _ := currentSettings.Effective["modelOverrides"].(map[string]any)

	return mergeModelCatalog(models, native, overrides), currentSettings, nil
}

func (c *Client) listModels(ctx context.Context) ([]AvailableModelInfo, error) {
	controller := c.activeController()
	if controller == nil {
		return nil, ErrClientNotStarted
	}

	response, err := controller.SendRequest(ctx, "list_models", nil, 3*time.Second)
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
	source, ok := c.transport.(interface{ LaunchEnvironment() map[string]string })
	if !ok || settings == nil || settings.Effective == nil || c.isClosed() {
		return ModelCatalogAccess{}, false
	}

	env := source.LaunchEnvironment()
	if !directAPISettingsMatch(settings.Effective, env) {
		return ModelCatalogAccess{}, false
	}

	access, eligible := resolveModelCatalogAccess(env)
	if access.OAuth {
		// Native bare mode ignores OAuth, including an explicit environment
		// token. Its equivalent SIMPLE switch uses Claude's truthy spellings.
		simple := strings.ToLower(strings.TrimSpace(env[EnvironmentKey("CLAUDE_CODE_SIMPLE")]))
		if c.options.Bare || simple == "1" || simple == "true" || simple == modelCatalogEnvYes || simple == "on" {
			return ModelCatalogAccess{}, false
		}
	}

	return access, eligible
}

func resolveModelCatalogAccess(env map[string]string) (ModelCatalogAccess, bool) {
	if directAPIHasRouteOverride(env) || strings.TrimSpace(env[EnvironmentKey("CLAUDE_CODE_API_BASE_URL")]) != "" {
		return ModelCatalogAccess{}, false
	}

	base := strings.TrimSpace(env[EnvironmentKey(directAPIBaseURLEnv)])
	if base != "" {
		endpoint, err := url.Parse(base)
		if err != nil || endpoint.Scheme != authLoginURLScheme || !strings.EqualFold(endpoint.Hostname(), "api.anthropic.com") ||
			(endpoint.Port() != "" && endpoint.Port() != "443") || endpoint.User != nil ||
			(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" ||
			endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
			return ModelCatalogAccess{}, false
		}
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
