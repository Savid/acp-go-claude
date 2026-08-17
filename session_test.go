package claudeacp

import (
	"context"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/permissions"
	"github.com/stretchr/testify/require"
)

func TestSavePermissionRulesDefaultRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	home := t.TempDir()
	rules := map[string]string{"Read": claude.BehaviorAllow}

	require.NoError(t, savePermissionRules(ctx, home, "session-1", rules))

	store := permissions.Store{ClaudeHome: home}
	loaded, err := store.Load(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, rules, loaded)
}
