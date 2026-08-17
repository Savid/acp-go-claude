package claudeacp

import (
	"context"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAgentPermissionRuleLoadBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessionID := acp.SessionId("session-1")
	agent := NewAgent(WithHome(t.TempDir()))
	_, ok := agent.cachedPermissionRules(sessionID)
	require.False(t, ok)

	require.NoError(t, savePermissionRules(ctx, agent.options.Home, sessionID, map[string]string{"Read": claude.BehaviorAllow}))
	rules, err := agent.permissionRulesForSession(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow}, rules)

	explicit, err := agent.permissionRulesForStart(ctx, "next", sessionStart{PermissionRules: map[string]string{"Write": claude.BehaviorDeny}})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Write": claude.BehaviorDeny}, explicit)

	badHome := string([]byte{0})
	loadErrAgent := NewAgent(WithHome(badHome))
	_, err = loadErrAgent.loadPermissionRules(ctx, sessionID)
	require.ErrorContains(t, err, "load permission rules")
	loadErrAgent.cachePermissionRules(sessionID, map[string]string{"Bash": claude.BehaviorAllow})
	rules, err = loadErrAgent.loadPermissionRules(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Bash": claude.BehaviorAllow}, rules)
	rules["Bash"] = claude.BehaviorDeny
	cached, ok := loadErrAgent.cachedPermissionRules(sessionID)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, cached["Bash"])
}
