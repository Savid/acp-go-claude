package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"

	"github.com/savid/acp-go-core/usage/anthropic"
	"github.com/stretchr/testify/require"
)

// The native answer carries fixed window members, null placeholders, and a
// generic list; only the list and the two account members are read.
func TestAccountUsageDecodesTheNativeAnswer(t *testing.T) {
	t.Parallel()

	var usage AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{
		"session": {"total_cost_usd": 0},
		"subscription_type": "enterprise",
		"rate_limits_available": true,
		"rate_limits": {
			"five_hour": {"utilization": 4, "resets_at": "2026-09-17T03:30:00.051309+00:00"},
			"seven_day_opus": null,
			"limits": [
				{"kind": "session", "group": "session", "percent": 4, "severity": "normal", "resets_at": "2026-09-17T03:30:00.051309+00:00", "scope": null, "is_active": false},
				{"kind": "weekly_scoped", "group": "weekly", "percent": 22, "resets_at": "2026-09-19T08:00:00.051625+00:00", "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": true}
			]
		},
		"behaviors": null
	}`), &usage))

	require.Equal(t, "enterprise", usage.Plan)
	require.True(t, usage.Available)
	require.Len(t, usage.Windows.Limits, 2)
	require.Equal(t, anthropic.Limit{Kind: "session", Percent: new(float64(4)), ResetsAt: "2026-09-17T03:30:00.051309+00:00"}, usage.Windows.Limits[0])
	require.Equal(t, "weekly_scoped", usage.Windows.Limits[1].Kind)
	require.Equal(t, &anthropic.Model{DisplayName: "Fable"}, usage.Windows.Limits[1].Scope.Model)

	var unauthenticated AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{"subscription_type": null, "rate_limits_available": false, "rate_limits": null, "behaviors": null}`), &unauthenticated))
	require.Equal(t, AccountUsage{}, unauthenticated)
}

// The control request names get_usage and always asks to skip behaviors.
func TestAccountUsageControlRequest(t *testing.T) {
	t.Parallel()

	toNative, fromAdapter := io.Pipe()
	toAdapter, fromNative := io.Pipe()
	client := NewClient(fromAdapter, toAdapter)
	client.Start(t.Context())

	go func() {
		defer fromNative.Close()

		var frame struct {
			Type      string         `json:"type"`
			RequestID string         `json:"request_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
			Request   map[string]any `json:"request"`
		}

		line, _ := bufio.NewReader(toNative).ReadBytes('\n')
		if json.Unmarshal(line, &frame) != nil || frame.Type != "control_request" || frame.Request[keySubtype] != "get_usage" || frame.Request["skip_behaviors"] != true {
			return
		}

		_ = json.NewEncoder(fromNative).Encode(map[string]any{keyType: typeControlResponse, keyResponse: map[string]any{
			keySubtype: "success", keyRequestID: frame.RequestID,
			keyResponse: map[string]any{"subscription_type": "pro", "rate_limits_available": true, "rate_limits": map[string]any{"limits": []any{map[string]any{"kind": "session", "percent": 9}}}},
		}})
	}()

	usage, err := client.AccountUsage(t.Context())
	require.NoError(t, err)
	require.Equal(t, "pro", usage.Plan)
	require.Equal(t, []anthropic.Limit{{Kind: "session", Percent: new(float64(9))}}, usage.Windows.Limits)
}
