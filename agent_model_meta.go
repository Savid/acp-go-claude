package claudeacp

import (
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	claudeModelMetaAvailableVariantsKey = "availableVariants"
	claudeModelMetaContextWindowKey     = "contextWindow"
	claudeModelMetaModelIDKey           = "modelId"
	claudeModelMetaSupportedEffortKey   = "supportedEffortLevels"
	claudeModelMetaSupportsAutoModeKey  = "supportsAutoMode"
	claudeModelMetaVariantKey           = "variant"

	claudeModelFamilyHaiku  = "haiku"
	claudeModelFamilyOpus   = "opus"
	claudeModelFamilySonnet = "sonnet"
)

// model_config follows the ACP model config category RFD until the SDK exposes
// a generated constant for it.
var modelConfigCategory = acp.SessionConfigOptionCategory("model_config")

func sessionResponseMeta(session *Session) map[string]any {
	session.mu.Lock()
	model := session.model
	available := append([]claude.AvailableModelInfo(nil), session.availableModels...)
	effort := session.effort
	session.mu.Unlock()

	return mergeAnyMap(
		claudeModelVariantMeta(model, available, effort),
		session.goalResponseMeta(),
	)
}

func (s *Session) goalResponseMeta() map[string]any {
	return map[string]any{
		claudeMetaKey: map[string]any{
			claudeGoalMetaKey: s.goalMetaValue(),
		},
	}
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

	if levels := nonEmptyModelStrings(info.SupportedEffortLevels); len(levels) > 0 {
		claudeMeta[claudeModelMetaSupportedEffortKey] = levels
	}

	if info.SupportsAutoMode {
		claudeMeta[claudeModelMetaSupportsAutoModeKey] = true
	}

	if contextWindow := modelContextWindowHint(info); contextWindow > 0 {
		claudeMeta[claudeModelMetaContextWindowKey] = contextWindow
	}

	if len(claudeMeta) == 0 {
		return nil
	}

	return map[string]any{claudeMetaKey: claudeMeta}
}

func modelContextWindowHint(info claude.AvailableModelInfo) int {
	if modelHasLargeContext(info.Value) {
		return largeContextWindow
	}

	text := modelHintText(info)
	if strings.Contains(text, "1m context") || strings.Contains(text, "1 million token") {
		return largeContextWindow
	}

	if strings.EqualFold(strings.TrimSpace(info.Value), modelTokenOpus) {
		return largeContextWindow
	}

	switch modelFamily(info) {
	case claudeModelFamilyHaiku, claudeModelFamilySonnet:
		return defaultContextWindow
	default:
		return 0
	}
}

func contextWindowForAvailableModel(model string, available []claude.AvailableModelInfo) int {
	if info, ok := availableModelInfo(model, available); ok {
		if contextWindow := modelContextWindowHint(info); contextWindow > 0 {
			return contextWindow
		}
	}

	if contextWindow := modelContextWindowHint(claude.AvailableModelInfo{Value: model}); contextWindow > 0 {
		return contextWindow
	}

	return contextWindowForModel(model)
}

func modelFamily(info claude.AvailableModelInfo) string {
	text := modelHintText(info)
	switch {
	case strings.Contains(text, claudeModelFamilyHaiku):
		return claudeModelFamilyHaiku
	case strings.Contains(text, claudeModelFamilySonnet):
		return claudeModelFamilySonnet
	case strings.Contains(text, claudeModelFamilyOpus):
		return claudeModelFamilyOpus
	default:
		return ""
	}
}

func modelHintText(info claude.AvailableModelInfo) string {
	return strings.ToLower(info.Value + " " + info.DisplayName + " " + info.Description)
}

func availableModelInfo(model string, available []claude.AvailableModelInfo) (claude.AvailableModelInfo, bool) {
	for _, info := range available {
		if info.Value == model {
			return info, true
		}
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

func configCategory(category acp.SessionConfigOptionCategory) *acp.SessionConfigOptionCategory {
	return &category
}
