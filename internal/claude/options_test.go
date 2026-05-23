package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPermissionDecisionPayload(t *testing.T) {
	t.Parallel()

	require.Equal(t, map[string]any{"behavior": BehaviorDeny}, PermissionDecision{}.toPayload(nil))

	allow := PermissionDecision{Behavior: BehaviorAllow}.toPayload(map[string]any{"path": "/tmp/a"})
	require.Equal(t, BehaviorAllow, allow["behavior"])
	require.Equal(t, map[string]any{"path": "/tmp/a"}, allow["updatedInput"])

	allowEmpty := PermissionDecision{Behavior: BehaviorAllow}.toPayload(nil)
	require.Equal(t, map[string]any{}, allowEmpty["updatedInput"])

	deny := PermissionDecision{
		Behavior:           BehaviorDeny,
		Message:            "no",
		Interrupt:          true,
		UpdatedPermissions: []map[string]any{{"rule": "x"}},
	}.toPayload(nil)

	require.Equal(t, BehaviorDeny, deny["behavior"])
	require.Equal(t, "no", deny["message"])
	require.Equal(t, true, deny["interrupt"])
	require.Equal(t, []map[string]any{{"rule": "x"}}, deny["updatedPermissions"])
}
