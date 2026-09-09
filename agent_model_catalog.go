package claudeacp

import (
	"context"
	"log/slog"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	nativeFamilySonnet = "sonnet"
	nativeFamilyOpus   = "opus"
	nativeFamilyHaiku  = "haiku"
	nativeFamilyFable  = "fable"
)

type modelCatalogService interface {
	claude.ModelCatalogReader
	Invalidate()
	Close()
}

func (a *Agent) discoverSessionModels(
	ctx context.Context,
	client *claude.Client,
) ([]claude.AvailableModelInfo, *claude.SettingsSnapshot, bool) {
	discoverCtx, finish := a.observe.StartClaudeProcess(ctx, "model_discovery")
	models, settings, err := client.DiscoverModels(discoverCtx, a.modelCatalog, a.options.DirectAPI)
	finish(err)

	if err != nil {
		a.log.DebugContext(ctx, "Claude model discovery unavailable", slog.String("stage", "model_discovery"))
	}

	known := settings != nil
	if settings == nil {
		settings = &claude.SettingsSnapshot{}
	}

	return models, settings, known
}

// reconcileSessionModels applies process-specific configuration after provider
// discovery. Each constraint filters the same catalog; none can widen another.
func reconcileSessionModels(
	catalog []claude.AvailableModelInfo,
	configured []string,
	settings *claude.SettingsSnapshot,
	overrides map[string]string,
) []claude.AvailableModelInfo {
	models := applyAvailableModelsAllowlist(catalog, configured)

	models = applyModelOverrides(models, catalog, overrides)
	if settings == nil || settings.Effective == nil {
		return models
	}

	raw, exists := settings.Effective[settingsFieldAvailableModels]
	if !exists {
		return models
	}

	allowlist, err := decodeStringSlice(raw, settingsFieldAvailableModels)
	if err != nil {
		allowlist = []string{}
	}

	for i := range models {
		selection := models[i]

		selection.Value = claudeModelID(selection.Value, overrides)
		if selection.Value != modelDefault && !nativeModelAllowed(selection, allowlist, catalog) {
			models[i].Disabled = true
		}
	}

	return models
}

// nativeModelAllowed applies the native availableModels grammar to model IDs.
// Specific entries narrow a family's wildcard; labels never establish identity.
func nativeModelAllowed(model claude.AvailableModelInfo, allowlist []string, catalog []claude.AvailableModelInfo) bool {
	value, resolved := nativeAllowlistID(model.Value), nativeAllowlistID(model.ResolvedModel)
	// Concrete Anthropic IDs retain their source identity under native
	// overrides. The override's target cannot admit an excluded source ID.
	if strings.HasPrefix(value, "claude-") {
		resolved = ""
	}

	entries := make([]string, len(allowlist))

	for i, entry := range allowlist {
		entries[i] = nativeAllowlistID(entry)
	}

	for _, entry := range entries {
		if entry == "" {
			continue
		}

		if nativeAllowlistFamily(entry) {
			if nativeAllowlistFamilyNarrowed(entry, entries) {
				continue
			}

			// Native aliases establish their exact resolved target as well.
			if nativeModelFamilyMatches(value, entry) || nativeModelFamilyMatches(resolved, entry) {
				return true
			}
		} else {
			for _, id := range []string{value, resolved} {
				if nativeAllowlistPrefixMatches(id, entry) {
					return true
				}
			}
		}

		if allowed := resolveModelPreference(catalog, entry); allowed != nil && allowed.ResolvedModel != "" &&
			(value == nativeAllowlistID(allowed.ResolvedModel) || resolved == nativeAllowlistID(allowed.ResolvedModel)) {
			return true
		}
	}

	return false
}

func nativeAllowlistID(id string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(id)), "[1m]")
}

func nativeAllowlistFamily(id string) bool {
	switch id {
	case nativeFamilySonnet, nativeFamilyOpus, nativeFamilyHaiku, nativeFamilyFable:
		return true
	default:
		return false
	}
}

func nativeAllowlistFamilyNarrowed(family string, entries []string) bool {
	for _, entry := range entries {
		if nativeAllowlistFamily(entry) {
			continue
		}

		if _, suffix, found := strings.Cut(entry, family); found && (suffix == "" || strings.HasPrefix(suffix, "-")) {
			return true
		}
	}

	return false
}

func nativeModelFamilyMatches(id, family string) bool {
	for offset := 0; offset < len(id); {
		index := strings.Index(id[offset:], family)
		if index < 0 {
			return false
		}

		index += offset

		end := index + len(family)
		if (index == 0 || !nativeModelIDAlphanumeric(id[index-1])) &&
			(end == len(id) || !nativeModelIDAlphanumeric(id[end])) {
			return true
		}

		offset = index + 1
	}

	return false
}

func nativeModelIDAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func nativeAllowlistPrefixMatches(id, entry string) bool {
	if id == entry || strings.HasPrefix(id, entry+"-") {
		return true
	}

	if strings.HasPrefix(entry, "claude-") {
		return false
	}

	return id == "claude-"+entry || strings.HasPrefix(id, "claude-"+entry+"-")
}

// Explicit overrides dispatch a different identity. Facts about the original
// alias are not evidence about that target, even when its name looks similar.
func applyModelOverrides(models, catalog []claude.AvailableModelInfo, overrides map[string]string) []claude.AvailableModelInfo {
	for i := range models {
		original := models[i]

		target := claudeModelID(original.Value, overrides)
		if target == original.Value {
			continue
		}

		mapped := claude.AvailableModelInfo{Value: original.Value, ResolvedModel: target, DisplayName: original.Value}
		if found := resolveModelPreference(catalog, target); found != nil {
			mapped = *found

			mapped.Value = original.Value
			if mapped.ResolvedModel == "" {
				mapped.ResolvedModel = target
			}
		}

		mapped.Disabled = original.Disabled || claude.ModelDisabled(target, catalog)
		models[i] = mapped
	}

	return models
}
