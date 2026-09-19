package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"

	"github.com/savid/acp-go-core/usage/anthropic"
	"github.com/stretchr/testify/require"
)

// The native answer carries fixed window members, null placeholders for
// windows the account lacks, model-scoped windows, and spending; the session
// and weekly windows, the scoped list, spending, and the two account members
// are read.
func TestAccountUsageDecodesTheNativeAnswer(t *testing.T) {
	t.Parallel()

	var usage AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{
		"session": {"total_cost_usd": 0},
		"subscription_type": "max",
		"rate_limits_available": true,
		"rate_limits": {
			"five_hour": {"utilization": 2, "resets_at": "2026-09-19T09:20:00.194239+00:00", "limit_dollars": null, "locked_reason": null},
			"seven_day": {"utilization": 0, "resets_at": "2026-09-25T05:00:00.194262+00:00"},
			"seven_day_opus": null,
			"seven_day_sonnet": null,
			"extra_usage": {"is_enabled": false, "utilization": null},
			"spend": {"used": {"amount_minor": 0, "currency": "AUD", "exponent": 2}, "limit": null, "percent": 0, "enabled": false},
			"model_scoped": [{"display_name": "Fable", "utilization": 1, "resets_at": "2026-09-25T05:00:00.194557+00:00"}]
		},
		"behaviors": null
	}`), &usage))

	require.Equal(t, "max", usage.Plan)
	require.True(t, usage.Available)
	require.Equal(t, &Window{Utilization: new(float64(2)), ResetsAt: "2026-09-19T09:20:00.194239+00:00"}, usage.Windows.Session)
	require.Equal(t, &Window{Utilization: new(float64(0)), ResetsAt: "2026-09-25T05:00:00.194262+00:00"}, usage.Windows.Weekly)
	require.Equal(t, []ScopedWindow{{Window: Window{Utilization: new(float64(1)), ResetsAt: "2026-09-25T05:00:00.194557+00:00"}, DisplayName: "Fable"}}, usage.Windows.ModelScoped)
	require.Equal(t, &anthropic.Spend{Used: &anthropic.Money{AmountMinor: new(float64(0)), Currency: "AUD", Exponent: new(int(2))}}, usage.Windows.Spend)

	require.Equal(t, anthropic.Observation{
		Limits: []anthropic.Limit{
			{Kind: "session", Percent: new(float64(2)), ResetsAt: "2026-09-19T09:20:00.194239+00:00"},
			{Kind: "weekly_all", Percent: new(float64(0)), ResetsAt: "2026-09-25T05:00:00.194262+00:00"},
			{Kind: "weekly_scoped", Percent: new(float64(1)), ResetsAt: "2026-09-25T05:00:00.194557+00:00", Scope: &anthropic.Scope{Model: &anthropic.Model{DisplayName: "Fable"}}},
		},
		Spend: usage.Windows.Spend,
	}, usage.Windows.Observation())

	var unauthenticated AccountUsage

	require.NoError(t, json.Unmarshal([]byte(`{"subscription_type": null, "rate_limits_available": false, "rate_limits": null, "behaviors": null}`), &unauthenticated))
	require.Equal(t, AccountUsage{}, unauthenticated)
}

// A window without a percentage, and a scoped window without one, are left
// out of the observation rather than failing it.
func TestRateLimitsObservationSkipsWindowsWithoutAPercentage(t *testing.T) {
	t.Parallel()

	limits := RateLimits{
		Session:     &Window{ResetsAt: "2026-09-19T09:20:00+00:00"},
		Weekly:      &Window{Utilization: new(float64(15))},
		ModelScoped: []ScopedWindow{{DisplayName: "Opus"}, {Window: Window{Utilization: new(float64(22))}, DisplayName: "Fable"}},
	}

	require.Equal(t, anthropic.Observation{Limits: []anthropic.Limit{
		{Kind: "weekly_all", Percent: new(float64(15))},
		{Kind: "weekly_scoped", Percent: new(float64(22)), Scope: &anthropic.Scope{Model: &anthropic.Model{DisplayName: "Fable"}}},
	}}, limits.Observation())
	require.Equal(t, anthropic.Observation{}, RateLimits{}.Observation())
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
			keyResponse: map[string]any{"subscription_type": "pro", "rate_limits_available": true, "rate_limits": map[string]any{"five_hour": map[string]any{"utilization": 9}}},
		}})
	}()

	usage, err := client.AccountUsage(t.Context())
	require.NoError(t, err)
	require.Equal(t, "pro", usage.Plan)
	require.Equal(t, &RateLimits{Session: &Window{Utilization: new(float64(9))}}, usage.Windows)
}
