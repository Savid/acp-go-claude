package claudeacp

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	envAnthropicModel       = "ANTHROPIC_MODEL"
	envClaudeModelConfig    = "CLAUDE_MODEL_CONFIG"
	modelDefault            = "default"
	modelTokenClaude        = "claude"
	modelTokenOpus          = "opus"
	modelTokenBest          = "best"
	modelTokenOpusPlan      = "opusplan"
	modelContextHintPattern = `(?i)\[(\d+m)\]$`
)

type modelConfig struct {
	ModelOverrides  map[string]string `json:"modelOverrides,omitempty"`
	AvailableModels []string          `json:"availableModels,omitempty"`
}

var modelContextHintRE = regexp.MustCompile(modelContextHintPattern)

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
	return parseModelConfig(envValue(env, envClaudeModelConfig))
}

func envValue(env map[string]string, key string) string {
	if value, ok := env[key]; ok {
		return value
	}

	return os.Getenv(key)
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
		return append([]claude.AvailableModelInfo(nil), available...)
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

func resolveModelPreference(
	models []claude.AvailableModelInfo,
	preference string,
) *claude.AvailableModelInfo {
	trimmed := strings.TrimSpace(preference)
	if trimmed == "" {
		return nil
	}

	lower := strings.ToLower(trimmed)
	for _, model := range models {
		if model.Value == trimmed ||
			strings.EqualFold(model.Value, trimmed) ||
			strings.EqualFold(model.DisplayName, trimmed) {
			return cloneModelInfo(model)
		}
	}

	for _, model := range models {
		value := strings.ToLower(model.Value)
		display := strings.ToLower(model.DisplayName)

		if strings.Contains(value, lower) || strings.Contains(display, lower) || strings.Contains(lower, value) {
			return cloneModelInfo(model)
		}
	}

	tokens, contextHint := tokenizeModelPreference(trimmed)
	if len(tokens) == 0 {
		return nil
	}

	var best *claude.AvailableModelInfo

	bestScore := 0
	for _, model := range models {
		if score := scoreModelMatch(model, tokens, contextHint); score > 0 && score > bestScore {
			best = cloneModelInfo(model)
			bestScore = score
		}
	}

	return best
}

func tokenizeModelPreference(model string) ([]string, string) {
	lower := strings.ToLower(strings.TrimSpace(model))

	contextHint := ""
	if match := modelContextHintRE.FindStringSubmatch(lower); len(match) > 1 {
		contextHint = match[1]
	}

	normalized := modelContextHintRE.ReplaceAllString(lower, " $1 ")
	fields := strings.FieldsFunc(normalized, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})

	tokens := make([]string, 0, len(fields))
	for _, token := range fields {
		switch token {
		case modelTokenOpusPlan:
			token = modelTokenOpus
		case modelTokenBest, modelDefault:
			token = ""
		}

		if token == "" || token == modelTokenClaude {
			continue
		}

		if strings.ContainsAny(token, "abcdefghijklmnopqrstuvwxyz") || strings.HasSuffix(token, "m") {
			tokens = append(tokens, token)
		}
	}

	return tokens, contextHint
}

func scoreModelMatch(model claude.AvailableModelInfo, tokens []string, contextHint string) int {
	haystack := strings.ToLower(model.Value + " " + model.DisplayName)
	score := 0

	for _, token := range tokens {
		if strings.Contains(haystack, token) {
			if token == contextHint {
				score += 3
			} else {
				score++
			}
		}
	}

	return score
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
