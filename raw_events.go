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

	rawEventFieldTruncated = "truncated"
	rawEventFieldReason    = "reason"
	rawEventFieldMaxBytes  = "maxBytes"
	rawEventFieldSizeBytes = "sizeBytes"

	rawEventReasonOversize       = "oversize"
	rawEventReasonUnserializable = "unserializable"
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

// rawEventMarker inspects the fully-marshalled notification payload and returns
// a fixed truncation marker when the event cannot be delivered verbatim:
// oversize when it exceeds the 64 KiB limit, unserializable when it fails to
// marshal. It returns (nil, false) when the event fits and can be sent as-is.
// Oversized events are never dropped; the marker consumes the sequence so the
// per-session sequence stays contiguous and self-describing.
func rawEventMarker(payload map[string]any) (map[string]any, bool) {
	data, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{
			rawEventFieldTruncated: true,
			rawEventFieldReason:    rawEventReasonUnserializable,
			rawEventFieldMaxBytes:  rawEventMaxBytes,
		}, true
	}

	if len(data) > rawEventMaxBytes {
		return map[string]any{
			rawEventFieldTruncated: true,
			rawEventFieldReason:    rawEventReasonOversize,
			rawEventFieldMaxBytes:  rawEventMaxBytes,
			rawEventFieldSizeBytes: len(data),
		}, true
	}

	return nil, false
}
