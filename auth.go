package claudeacp

import (
	"maps"
)

func cloneStringMap(values map[string]string) map[string]string {
	return maps.Clone(values)
}

func clientMetaBool(meta map[string]any, key string) bool {
	value, _ := meta[key].(bool)

	return value
}
