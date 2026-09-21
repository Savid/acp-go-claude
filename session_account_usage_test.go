package claudeacp

import (
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAccountUsageSharesCachedWindowsAcrossSessionsAndObservesTurnFailures(t *testing.T) {
	h := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvAccountUsage: "unavailable", "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}))
	h.initialize()
	first := h.newSession().SessionId
	s, err := h.agent.session(h.ctx(), first)
	require.NoError(t, err)
	s.mu.Lock()
	access := s.runtime.quotaAccess
	s.mu.Unlock()
	require.NotNil(t, access)
	now := time.Now().UTC().Truncate(time.Second)
	fable := claude.QuotaWindow{ID: claude.QuotaFable, Percent: 99, ObservedAt: now, RefreshAt: now.Add(30 * time.Minute), ResetsAt: now.Add(7 * 24 * time.Hour)}
	h.agent.quota.Observe(access.Key(), []claude.QuotaWindow{
		{ID: claude.QuotaSession, Percent: 90, ObservedAt: now, RefreshAt: now.Add(5 * time.Minute), ResetsAt: now.Add(time.Hour)}, fable,
	}, "")
	before, err := callAccountUsage(t, h, map[string]any{"providerId": "anthropic", accountUsageSessionField: first})
	require.NoError(t, err)
	second := h.newSession().SessionId
	again, err := callAccountUsage(t, h, map[string]any{"providerId": "anthropic", accountUsageSessionField: second})
	require.NoError(t, err)
	require.Equal(t, before, again)
	_, err = h.conn.Prompt(h.ctx(), acp.PromptRequest{SessionId: first, Prompt: []acp.ContentBlock{acp.TextBlock("QUOTA_SESSION_EXHAUSTED")}})
	require.ErrorContains(t, err, "usage limit")
	after, err := callAccountUsage(t, h, map[string]any{"providerId": "anthropic", accountUsageSessionField: second})
	require.NoError(t, err)
	require.Equal(t, 100.0, after.Limits[0].UsedPercent)
	require.Equal(t, before.Limits[1], after.Limits[1])
}
