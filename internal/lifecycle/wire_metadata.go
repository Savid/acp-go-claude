package lifecycle

import (
	"bytes"
	"encoding/json"
	"strings"
)

// RetainRequestMetadata preserves only the owned lifecycle value after the SDK
// request decode. Validation still runs at the method's original semantic stage.
// Foreign metadata retains the SDK's representation and interpretation.
func RetainRequestMetadata(meta map[string]any, params json.RawMessage) map[string]any {
	var value json.RawMessage

	metaCount, ownedCount := 0, 0

	visitWireObject(params, func(field string, raw json.RawMessage) {
		if !strings.EqualFold(field, metaField) {
			return
		}

		metaCount++

		visitWireObject(raw, func(field string, raw json.RawMessage) {
			if field == MetaKey {
				ownedCount++
				value = raw
			}
		})
	})

	if ownedCount == 0 {
		return meta
	}

	if meta == nil {
		meta = make(map[string]any)
	}

	if metaCount > 1 || ownedCount > 1 {
		meta[MetaKey] = paramError()
	} else {
		meta[MetaKey] = value
	}

	return meta
}

// visitWireObject scans already-valid request JSON without imposing duplicate
// rules on unrelated objects. The owned validator decides any refusal later.
func visitWireObject(raw json.RawMessage, visit func(string, json.RawMessage)) {
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
		visit(field, value)
	}
}

func negotiationObject(value any, members ...string) (map[string]any, *ParamError) {
	switch value := value.(type) {
	case map[string]any:
		return value, nil
	case *ParamError:
		return nil, value
	case json.RawMessage:
		if !json.Valid(value) || bytes.TrimSpace(value)[0] != '{' {
			return nil, paramError(members...)
		}

		fields := make(map[string]any)

		var refusal *ParamError

		visitWireObject(value, func(field string, raw json.RawMessage) {
			if _, duplicate := fields[field]; duplicate && refusal == nil {
				refusal = paramError(append(members, field)...)
			}

			fields[field] = raw
		})

		return fields, refusal
	default:
		return nil, paramError(members...)
	}
}
