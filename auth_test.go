package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCloneStringMap(t *testing.T) {
	t.Parallel()

	require.Nil(t, cloneStringMap(nil))

	original := map[string]string{"key": "value"}
	cloned := cloneStringMap(original)
	require.Equal(t, original, cloned)

	cloned["key"] = "changed"
	require.Equal(t, "value", original["key"])
}

func TestClientMetaBool(t *testing.T) {
	t.Parallel()

	const metaKey = "key"

	require.False(t, clientMetaBool(nil, "missing"))
	require.False(t, clientMetaBool(map[string]any{metaKey: "not-bool"}, metaKey))
	require.False(t, clientMetaBool(map[string]any{metaKey: false}, metaKey))
	require.True(t, clientMetaBool(map[string]any{metaKey: true}, metaKey))
}
