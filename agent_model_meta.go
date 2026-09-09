package claudeacp

import (
	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	claudeModelMetaAvailableVariantsKey = "availableVariants"
	claudeModelMetaContextWindowKey     = "contextWindow"
	claudeModelMetaModelIDKey           = "modelId"
	claudeModelMetaMaxOutputTokensKey   = "maxOutputTokens"
	claudeModelMetaSupportedEffortKey   = "supportedEffortLevels"
	claudeModelMetaSupportsAutoModeKey  = "supportsAutoMode"
	claudeModelMetaVariantKey           = "variant"
)

// model_config follows the ACP model config category RFD until the SDK exposes
// a generated constant for it.
var modelConfigCategory = acp.SessionConfigOptionCategory("model_config")

func sessionResponseMeta(session *agentSession) map[string]any {
	return sessionResponseMetaWithInjection(session, "")
}

func sessionReuseResponseMeta(session *agentSession, bindings map[string]ProviderAuthBinding) map[string]any {
	if len(bindings) == 0 {
		return sessionResponseMeta(session)
	}

	session.mu.Lock()
	applied := session.providerAuthInjection == authInjectionApplied
	session.mu.Unlock()

	if applied {
		return sessionResponseMetaWithInjection(session, authInjectionNoop)
	}

	return sessionResponseMeta(session)
}

func sessionLoadResponseMeta(
	session *agentSession,
	bindings map[string]ProviderAuthBinding,
	started bool,
) map[string]any {
	if started {
		return sessionResponseMeta(session)
	}

	return sessionReuseResponseMeta(session, bindings)
}

func sessionResponseMetaWithInjection(session *agentSession, override string) map[string]any {
	session.mu.Lock()
	model := session.model
	available := claude.CloneAvailableModels(session.availableModels)
	effort := session.effort
	injection := session.providerAuthInjection
	session.mu.Unlock()

	if override != "" {
		injection = override
	}

	meta := claudeModelVariantMeta(model, available, effort)
	if injection == "" {
		return meta
	}

	if meta == nil {
		meta = map[string]any{}
	}

	claudeMeta, _ := meta[claudeMetaKey].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = map[string]any{}
		meta[claudeMetaKey] = claudeMeta
	}

	claudeMeta[providerAuthCapabilityKey] = map[string]any{"injection": injection}

	return meta
}

func claudeModelVariantMeta(model string, available []claude.AvailableModelInfo, effort string) map[string]any {
	if model == "" {
		return nil
	}

	variant := any(nil)
	if effort != "" {
		variant = effort
	}

	return map[string]any{
		claudeMetaKey: map[string]any{
			claudeModelMetaModelIDKey:           model,
			claudeModelMetaVariantKey:           variant,
			claudeModelMetaAvailableVariantsKey: nonEmptyModelStrings(effortLevelsForModel(model, available)),
		},
	}
}

func claudeModelInfoMeta(info claude.AvailableModelInfo) map[string]any {
	claudeMeta := make(map[string]any)

	if levels := nonEmptyModelStrings(info.SupportedEffortLevels); len(levels) > 0 && !info.EffortUnsupported {
		claudeMeta[claudeModelMetaSupportedEffortKey] = levels
	}

	if info.SupportsAutoMode {
		claudeMeta[claudeModelMetaSupportsAutoModeKey] = true
	}

	if info.ContextWindow > 0 {
		claudeMeta[claudeModelMetaContextWindowKey] = info.ContextWindow
	}

	if info.MaxOutputTokens > 0 {
		claudeMeta[claudeModelMetaMaxOutputTokensKey] = info.MaxOutputTokens
	}

	if len(claudeMeta) == 0 {
		return nil
	}

	return map[string]any{claudeMetaKey: claudeMeta}
}

func availableModelInfo(model string, available []claude.AvailableModelInfo) (claude.AvailableModelInfo, bool) {
	if info := resolveModelPreference(available, model); info != nil {
		return *info, true
	}

	return claude.AvailableModelInfo{}, false
}

func nonEmptyModelStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		if value == "" {
			continue
		}

		if _, ok := seen[value]; ok {
			continue
		}

		result = append(result, value)
		seen[value] = struct{}{}
	}

	return result
}
