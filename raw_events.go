package claudeacp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

const (
	rawEventMaxBytes = 64 * 1024

	claudeMetaKey           = "claude"
	structuredOutputMetaKey = "structuredOutput"
	usageMetaKey            = "usage"
	rawMessageOriginKey     = "origin"
	rawEventFieldEvent      = "event"
	rawEventFieldSequence   = "sequence"
	rawEventFieldSource     = "source"

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

	sanitized, _ := sanitizeDiagnosticValue(msg.RawMessage()).(map[string]any)

	return sanitized
}

func sanitizeDiagnosticValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed)+1)
		for key, item := range typed {
			if key == jsonFieldURL || key == jsonFieldURI {
				if text, ok := item.(string); ok {
					cloned[key] = redactDiagnosticURI(text)

					continue
				}
			}

			cloned[key] = sanitizeDiagnosticValue(item)
		}

		if imageType, _ := typed[jsonFieldType].(string); imageType == imageContentType {
			sanitizeDiagnosticImage(cloned, typed)
		}

		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = sanitizeDiagnosticValue(item)
		}

		return cloned
	default:
		return value
	}
}

func sanitizeDiagnosticImage(cloned map[string]any, original map[string]any) {
	data, mimeType := diagnosticImageData(original)
	if data == "" {
		return
	}

	delete(cloned, jsonFieldData)

	if source, ok := cloned["source"].(map[string]any); ok {
		delete(source, jsonFieldData)
	}

	metadata := map[string]any{"mimeType": mimeType}

	if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
		sum := sha256.Sum256(decoded)
		metadata["sizeBytes"] = len(decoded)
		metadata["sha256"] = hex.EncodeToString(sum[:])

		if info, ok := mapper.InspectRaster(decoded); ok {
			metadata["width"] = info.Width
			metadata["height"] = info.Height
		}
	}

	cloned["imageMetadata"] = metadata
}

func diagnosticImageData(raw map[string]any) (string, string) {
	data, _ := raw[jsonFieldData].(string)

	mimeType, _ := raw[jsonFieldMediaType].(string)
	if mimeType == "" {
		mimeType, _ = raw["mimeType"].(string)
	}

	source, _ := raw["source"].(map[string]any)
	if source != nil {
		if data == "" {
			data, _ = source[jsonFieldData].(string)
		}

		if mimeType == "" {
			mimeType, _ = source[jsonFieldMediaType].(string)
		}
	}

	return data, mimeType
}

func redactDiagnosticURI(value string) string {
	if strings.HasPrefix(value, "data:") {
		return "data:[redacted]"
	}

	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != uriSchemeHTTP && parsed.Scheme != uriSchemeHTTPS) {
		return value
	}

	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String()
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

// capRawEventPayload replaces only the provider event when the complete
// routed payload is oversized, then proves the marker payload itself fits.
// Valid inbound routes are bounded so this final check is defensive rather
// than a reason to discard a native event.
func capRawEventPayload(payload map[string]any) (map[string]any, error) {
	if marker, replaced := rawEventMarker(payload); replaced {
		payload[rawEventFieldEvent] = marker
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal capped raw event payload: %w", err)
	}

	if len(data) > rawEventMaxBytes {
		return nil, fmt.Errorf("capped raw event payload is %d bytes, exceeds %d", len(data), rawEventMaxBytes)
	}

	return payload, nil
}
