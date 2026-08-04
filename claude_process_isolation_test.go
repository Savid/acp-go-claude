package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeProcessIsolationClonesPolicy(t *testing.T) {
	base := map[string]string{"PATH": "/policy/bin", "CANARY": "base"}
	policy := &ProcessIsolation{UID: 12, GID: 34, BaseEnvironment: base}

	converted := claudeProcessIsolation(policy)
	base["CANARY"] = "mutated"

	require.Equal(t, uint32(12), converted.UID)
	require.Equal(t, uint32(34), converted.GID)
	require.Equal(t, "base", converted.BaseEnvironment["CANARY"])
	require.Nil(t, claudeProcessIsolation(nil))
}
