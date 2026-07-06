package claudeacp

import (
	"encoding/json"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	rawEventMaxBytes = 64 * 1024

	claudeMetaKey           = "claude"
	structuredOutputMetaKey = "structuredOutput"
	usageMetaKey            = "usage"
	rawMessageTypeKey       = "type"
	rawMessageOriginKey     = "origin"
	rawEventFieldEvent      = "event"
)

type rawMessageConfig struct {
	All bool
}

func rawMessageConfigFromMeta(meta map[string]any) rawMessageConfig {
	claudeMeta, _ := meta[claudeMetaKey].(map[string]any)
	if claudeMeta == nil {
		return rawMessageConfig{}
	}

	raw, _ := claudeMeta[metaRawEventKey].(map[string]any)

	enabled, _ := raw[metaRawEventEnabledKey].(bool)
	if enabled {
		return rawMessageConfig{All: true}
	}

	return rawMessageConfig{}
}

func (c rawMessageConfig) Enabled() bool {
	return c.All
}

func (c rawMessageConfig) ShouldEmit(raw map[string]any) bool {
	if raw == nil {
		return false
	}

	return c.All
}

func rawClaudeMessage(msg claude.Message) map[string]any {
	if msg == nil {
		return nil
	}

	return msg.RawMessage()
}

func rawEventWithinLimit(payload any) bool {
	data, err := json.Marshal(payload)

	return err == nil && len(data) <= rawEventMaxBytes
}
