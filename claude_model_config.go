package claudeacp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	envAnthropicModel    = "ANTHROPIC_MODEL"
	envClaudeModelConfig = "CLAUDE_MODEL_CONFIG"
	modelDefault         = "default"
)

type modelConfig struct {
	ModelOverrides  map[string]string `json:"modelOverrides,omitempty"`
	AvailableModels []string          `json:"availableModels,omitempty"`
}

func parseModelConfig(raw string) (modelConfig, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return modelConfig{}, false, nil
	}

	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return modelConfig{}, false, err
	}

	object, ok := decoded.(map[string]any)
	if !ok {
		return modelConfig{}, false, fmt.Errorf("%s must be a JSON object", envClaudeModelConfig)
	}

	config := modelConfig{}

	if rawOverrides, ok := object["modelOverrides"]; ok {
		overrides, err := decodeStringMap(rawOverrides, "modelOverrides")
		if err != nil {
			return modelConfig{}, false, err
		}

		config.ModelOverrides = overrides
	}

	if rawAvailable, ok := object["availableModels"]; ok {
		available, err := decodeStringSlice(rawAvailable, "availableModels")
		if err != nil {
			return modelConfig{}, false, err
		}

		config.AvailableModels = available
	}

	if len(config.ModelOverrides) == 0 && config.AvailableModels == nil {
		return modelConfig{}, false, nil
	}

	return config, true, nil
}

func modelConfigFromEnv(env map[string]string) (modelConfig, bool, error) {
	return parseModelConfig(env[claude.EnvironmentKey(envClaudeModelConfig)])
}

func decodeStringMap(value any, field string) (map[string]string, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}

	result := make(map[string]string, len(raw))
	for key, value := range raw {
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s values must be strings", field)
		}

		result[key] = str
	}

	return result, nil
}

func decodeStringSlice(value any, field string) ([]string, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", field)
	}

	result := make([]string, 0, len(values))
	for _, value := range values {
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s values must be strings", field)
		}

		result = append(result, str)
	}

	return result, nil
}

func applyAvailableModelsAllowlist(
	available []claude.AvailableModelInfo,
	allowlist []string,
) []claude.AvailableModelInfo {
	if allowlist == nil {
		return claude.CloneAvailableModels(available)
	}

	result := make([]claude.AvailableModelInfo, 0, len(allowlist)+1)
	defaultModel := defaultModelInfo(available)
	result = append(result, defaultModel)
	seen := map[string]struct{}{defaultModel.Value: {}}

	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		if _, ok := seen[entry]; ok {
			continue
		}

		info := claude.AvailableModelInfo{
			Value:       entry,
			DisplayName: entry,
		}
		if match := resolveModelPreference(withoutDefaultModel(available), entry); match != nil {
			info = *match
			info.Value = entry
		}

		result = append(result, info)
		seen[entry] = struct{}{}
	}

	// Retain explicit native refusals internally, including IDs outside the
	// allowlist. Projection and selection must not reintroduce them.
	for _, info := range available {
		if info.Disabled {
			if _, exists := seen[info.Value]; !exists {
				result = append(result, *cloneModelInfo(info))
				seen[info.Value] = struct{}{}
			}
		}
	}

	return result
}

func defaultModelInfo(available []claude.AvailableModelInfo) claude.AvailableModelInfo {
	for _, info := range available {
		if info.Value == modelDefault {
			return info
		}
	}

	return claude.AvailableModelInfo{Value: modelDefault, DisplayName: modeNameDefault}
}

func withoutDefaultModel(available []claude.AvailableModelInfo) []claude.AvailableModelInfo {
	filtered := make([]claude.AvailableModelInfo, 0, len(available))
	for _, info := range available {
		if info.Value != modelDefault {
			filtered = append(filtered, info)
		}
	}

	return filtered
}

// resolveModelPreference preserves exact native aliases and full model IDs.
// Similar names and neighboring versions are never interchangeable.
func resolveModelPreference(models []claude.AvailableModelInfo, preference string) *claude.AvailableModelInfo {
	value := strings.TrimSpace(preference)
	if value == "" {
		return nil
	}

	for _, model := range models {
		if model.Value == value {
			return cloneModelInfo(model)
		}
	}

	for _, model := range models {
		if model.ResolvedModel == value {
			resolved := cloneModelInfo(model)
			resolved.Value = value

			return resolved
		}
	}

	return nil
}

func cloneModelInfo(model claude.AvailableModelInfo) *claude.AvailableModelInfo {
	cloned := model
	cloned.SupportedEffortLevels = append([]string(nil), model.SupportedEffortLevels...)

	return &cloned
}

func claudeModelID(model string, overrides map[string]string) string {
	if override := overrides[model]; override != "" {
		return override
	}

	return model
}
