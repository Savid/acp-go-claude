package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

const (
	rawClaudeSDKMessageMethod = "_claude/sdkMessage"

	claudeMetaKey               = "claude"
	emitRawSDKMessagesKey       = "emitRawSDKMessages"
	outputFormatCapabilityKey   = "outputFormat"
	outputFormatConfigPath      = "_meta.claude.options.outputFormat"
	outputFormatResultPath      = "usage_update._meta.claude.structuredOutput"
	outputFormatResultKey       = "result"
	rawSDKMessagesCapabilityKey = "sdkMessages"
	rawSDKMessagesMethodKey     = "method"
	rawSDKMessagesEnabledByKey  = "enabledBy"
	rawSDKMessagesEnabledByPath = "_meta.claude.emitRawSDKMessages"
	structuredOutputMetaKey     = "structuredOutput"
	usageMetaKey                = "usage"
	capabilityScopeKey          = "scope"
	capabilityScopeSession      = "session"
	rawMessageTypeKey           = "type"
	rawMessageControlRequest    = "control_request"
	rawMessageControlResponse   = "control_response"
	rawMessageSubtypeKey        = "subtype"
	rawMessageOriginKey         = "origin"
	rawMessageOriginKindKey     = "kind"
)

type rawMessageConfig struct {
	All     bool
	Filters []rawMessageFilter
}

type rawMessageFilter struct {
	Type    string
	Subtype string
	Origin  string
}

func rawMessageConfigFromMeta(meta map[string]any) rawMessageConfig {
	claudeMeta, _ := meta[claudeMetaKey].(map[string]any)
	if claudeMeta == nil {
		return rawMessageConfig{}
	}

	if config, ok := rawMessageConfigFromValue(claudeMeta[emitRawSDKMessagesKey]); ok {
		return config
	}

	return rawMessageConfig{}
}

func rawMessageConfigFromValue(value any) (rawMessageConfig, bool) {
	switch typed := value.(type) {
	case bool:
		return rawMessageConfig{All: typed}, true
	case []any:
		return rawMessageConfig{Filters: rawMessageFiltersFromAny(typed)}, true
	default:
		return rawMessageConfig{}, false
	}
}

func rawMessageFiltersFromAny(values []any) []rawMessageFilter {
	filters := make([]rawMessageFilter, 0, len(values))
	for _, value := range values {
		raw, _ := value.(map[string]any)
		if filter, ok := rawMessageFilterFromMap(raw); ok {
			filters = append(filters, filter)
		}
	}

	return filters
}

func rawMessageFilterFromMap(raw map[string]any) (rawMessageFilter, bool) {
	if raw == nil {
		return rawMessageFilter{}, false
	}

	filter := rawMessageFilter{
		Type:    rawStringValue(raw, rawMessageTypeKey),
		Subtype: rawStringValue(raw, rawMessageSubtypeKey),
		Origin:  rawStringValue(raw, rawMessageOriginKey),
	}
	if filter.Type == "" {
		return rawMessageFilter{}, false
	}

	return filter, true
}

func (c rawMessageConfig) Enabled() bool {
	return c.All || len(c.Filters) > 0
}

func (c rawMessageConfig) ShouldEmit(raw map[string]any) bool {
	if raw == nil {
		return false
	}

	if internalRawMessage(raw) {
		return false
	}

	if c.All {
		return true
	}

	for _, filter := range c.Filters {
		if filter.Matches(raw) {
			return true
		}
	}

	return false
}

func internalRawMessage(raw map[string]any) bool {
	switch rawStringValue(raw, rawMessageTypeKey) {
	case rawMessageControlRequest, rawMessageControlResponse:
		return true
	default:
		return false
	}
}

func (f rawMessageFilter) Matches(raw map[string]any) bool {
	if f.Type == "" || rawStringValue(raw, rawMessageTypeKey) != f.Type {
		return false
	}

	if f.Subtype != "" && rawStringValue(raw, rawMessageSubtypeKey) != f.Subtype {
		return false
	}

	if f.Origin != "" && rawOriginKind(raw) != f.Origin {
		return false
	}

	return true
}

func rawOriginKind(raw map[string]any) string {
	origin, _ := raw[rawMessageOriginKey].(map[string]any)

	return rawStringValue(origin, rawMessageOriginKindKey)
}

func rawClaudeMessage(msg claude.Message) map[string]any {
	if msg == nil {
		return nil
	}

	return msg.RawMessage()
}

func rawClaudeJSON(msg claude.Message) string {
	if msg == nil {
		return ""
	}

	return msg.RawJSON()
}

func rawStringValue(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}

	value, _ := raw[key].(string)

	return value
}
