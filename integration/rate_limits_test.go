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

// TestClaudeRateLimitsLive exercises a structured native control read without a prompt.
func TestClaudeRateLimitsLive(t *testing.T) {
	parallelWhenPortableClaudeAuth(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})
	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	raw, err := conn.CallExtension(ctx, claudeacp.RateLimitsMethod, claudeacp.RateLimitsRequest{SessionID: session.SessionId})
	require.NoError(t, err)
	var result claudeacp.RateLimitsResponse
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, "anthropic", result.ProviderID)
	require.Contains(t, []string{"available", "unavailable", "unsupported"}, result.Availability)
	require.NotNil(t, result.Pools)
	if result.Availability == "available" {
		require.NotEmpty(t, result.Pools)
	} else {
		require.Empty(t, result.Pools)
	}
	for _, pool := range result.Pools {
		require.NotEmpty(t, pool.ID)
		require.NotEmpty(t, pool.Windows)
		for _, window := range pool.Windows {
			require.NotNil(t, window.UsedPercent)
			require.GreaterOrEqual(t, *window.UsedPercent, 0.0)
			_, parseErr := time.Parse(time.RFC3339Nano, window.ObservedAt)
			require.NoError(t, parseErr)
		}
	}
}
