//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

// TestClaudeRateLimitsLive exercises `_claude/rateLimits` against the real
// local Claude CLI. Assertions stay robust to account state: subscription
// accounts report usage windows, API-billing accounts legitimately report
// none — the wire shape must hold either way. This test exists to catch
// upstream changes to the `claude /usage` output format.
func TestClaudeRateLimitsLive(t *testing.T) {
	parallelWhenPortableClaudeAuth(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	raw, err := conn.CallExtension(ctx, claudeacp.RateLimitsMethod, struct{}{})
	require.NoError(t, err)

	var resp claudeacp.RateLimitsResponse
	require.NoError(t, json.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Windows)

	seen := make(map[string]struct{}, len(resp.Windows))
	for _, window := range resp.Windows {
		require.NotEmpty(t, window.ID)
		require.NotContains(t, seen, window.ID)
		seen[window.ID] = struct{}{}

		require.GreaterOrEqual(t, window.UsedPercent, 0.0)
		require.LessOrEqual(t, window.UsedPercent, 100.0)

		if window.ResetsAt != "" {
			resetsAt, parseErr := time.Parse(time.RFC3339, window.ResetsAt)
			require.NoError(t, parseErr)
			require.True(t, resetsAt.After(time.Now().Add(-24*time.Hour)))
		}
	}
}
