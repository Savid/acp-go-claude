package claudeacp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/savid/acp-go-claude/internal/mapper"
)

type requestWireMetadata struct {
	route    any
	handoffs map[int]any
}

type wireValueSpan struct{ start, end int }

// preserveLocalRequestMetadata keeps owned numbers out of the SDK's float64
// decoder. Only owned values are replaced, leaving unrelated member order,
// duplicates, and map merging intact. Their validators still run later.
func preserveLocalRequestMetadata(params json.RawMessage, route, prompt bool) (json.RawMessage, requestWireMetadata) {
	var retained requestWireMetadata
	if !json.Valid(params) {
		return params, retained
	}

	var spans []wireValueSpan

	visitRequestWireObject(params, 0, func(field string, raw json.RawMessage, start int) {
		if strings.EqualFold(field, "_meta") {
			visitRequestWireObject(raw, start, func(key string, value json.RawMessage, offset int) {
				switch {
				case key == lifecycle.MetaKey:
					spans = append(spans, wireValueSpan{offset, offset + len(value)})
				case route && key == routeMetaKey:
					retained.route = decodeWireNumberValue(value)
					spans = append(spans, wireValueSpan{offset, offset + len(value)})
				}
			})
		}

		if prompt && strings.EqualFold(field, "prompt") {
			retained.handoffs = retainWireHandoffs(raw, start, &spans)
		}
	})

	if len(spans) == 0 {
		return params, retained
	}

	safe := make([]byte, 0, len(params))

	previous := 0
	for _, span := range spans {
		safe = append(safe, params[previous:span.start]...)
		safe = append(safe, "null"...)
		previous = span.end
	}

	safe = append(safe, params[previous:]...)

	return safe, retained
}

func retainWireHandoffs(raw json.RawMessage, base int, spans *[]wireValueSpan) map[int]any {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	opening, _ := decoder.Token()
	if opening != json.Delim('[') {
		return nil
	}

	retained := make(map[int]any)

	for index := 0; decoder.More(); index++ {
		var block json.RawMessage

		_ = decoder.Decode(&block)
		offset := base + int(decoder.InputOffset()) - len(block)

		var fields map[string]json.RawMessage

		_ = json.Unmarshal(block, &fields)

		var kind string

		_ = json.Unmarshal(fields["type"], &kind)
		if kind != imageContentType {
			continue
		}

		visitRequestWireObject(block, offset, func(field string, metadata json.RawMessage, start int) {
			if !strings.EqualFold(field, "_meta") {
				return
			}

			visitRequestWireObject(metadata, start, func(key string, value json.RawMessage, start int) {
				if key == mapper.MetaKeyHandoff {
					retained[index] = decodeWireNumberValue(value)
					*spans = append(*spans, wireValueSpan{start, start + len(value)})
				}
			})
		})
	}

	return retained
}

// The caller has checked JSON syntax. Offsets identify only complete values;
// scanning does not impose a uniqueness policy on foreign objects.
func visitRequestWireObject(raw json.RawMessage, base int, visit func(string, json.RawMessage, int)) {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	opening, _ := decoder.Token()
	if opening != json.Delim('{') {
		return
	}

	for decoder.More() {
		token, _ := decoder.Token()
		field, _ := token.(string)

		var value json.RawMessage

		_ = decoder.Decode(&value)
		visit(field, value, base+int(decoder.InputOffset())-len(value))
	}
}

func decodeWireNumberValue(raw json.RawMessage) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any

	_ = decoder.Decode(&value)

	return value
}

func (r requestWireMetadata) restoreRoute(meta map[string]any) {
	if _, present := meta[routeMetaKey]; present {
		meta[routeMetaKey] = r.route
	}
}

func (r requestWireMetadata) restoreHandoffs(prompt []acp.ContentBlock) {
	for index, value := range r.handoffs {
		if index < len(prompt) && prompt[index].Image != nil {
			meta := prompt[index].Image.Meta
			if _, present := meta[mapper.MetaKeyHandoff]; present {
				meta[mapper.MetaKeyHandoff] = value
			}
		}
	}
}
