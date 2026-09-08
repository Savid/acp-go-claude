package claude

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadRateLimitsUsesControlOnly(t *testing.T) {
	t.Parallel()
	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)
	go autoRespondInitialize(transport)
	startClientForTest(t, client)
	go respondToControlRequestWithResponse(transport, "get_usage", map[string]any{
		"rate_limits_available": true, "subscription_type": "max",
		"rate_limits": map[string]any{"five_hour": map[string]any{"utilization": 42.5}},
	})
	result, err := client.ReadRateLimits(t.Context())
	require.NoError(t, err)
	require.Len(t, result.Pools, 1)
	require.Equal(t, 42.5, result.Pools[0].Windows[0].UsedPercent)
	require.Equal(t, "max", result.PlanType)
	frames := transport.sentPayloads()
	require.Len(t, frames, 2)
	raw, err := json.Marshal(frames[1])
	require.NoError(t, err)
	var frame map[string]any
	require.NoError(t, json.Unmarshal(raw, &frame))
	require.Equal(t, "control_request", frame["type"])
	require.Equal(t, map[string]any{"subtype": "get_usage", "skip_behaviors": true}, frame["request"])
}

func TestReadRateLimitsNativeAvailability(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		payload     map[string]any
		unsupported bool
		failed      bool
	}{
		{name: "supported empty snapshot", payload: map[string]any{"rate_limits_available": true, "rate_limits": nil}},
		{name: "native context unsupported", payload: map[string]any{"rate_limits_available": false}, unsupported: true},
		{name: "incompatible control response", payload: map[string]any{}, failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			transport := newFakeTransport()
			client := NewClient(nil, Options{}, transport)
			go autoRespondInitialize(transport)
			startClientForTest(t, client)
			go respondToControlRequestWithResponse(transport, "get_usage", test.payload)
			result, err := client.ReadRateLimits(t.Context())
			if test.failed {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.unsupported, result.Unsupported)
			require.Empty(t, result.Pools)
		})
	}
}

func TestParseRateLimitsPreservesPoolsAndOmitsUnusableFacts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	result := parseRateLimits(map[string]any{
		"five_hour":        map[string]any{"utilization": 125.5, "resets_at": "2026-09-08T12:00:00+10:00"},
		"seven_day":        map[string]any{"utilization": 0.0},
		"seven_day_opus":   map[string]any{"utilization": 42.0},
		"seven_day_sonnet": map[string]any{"utilization": 2.0, "resets_at": "2026-09-07T12:00:00Z"},
		"model_scoped":     []any{map[string]any{"display_name": "special", "utilization": 3.0}, map[string]any{"display_name": "special", "utilization": 9.0}},
		"extra_usage":      map[string]any{"used_credits": 50.0},
	}, now)
	require.Len(t, result.Pools, 3)
	require.Equal(t, "subscription", result.Pools[0].ID)
	require.Equal(t, 125.5, result.Pools[0].Windows[0].UsedPercent)
	require.Equal(t, now.Add(2*time.Hour), result.Pools[0].Windows[0].ResetsAt)
	require.Equal(t, 0.0, result.Pools[0].Windows[1].UsedPercent)
	require.Equal(t, "seven_day_opus", result.Pools[1].ID)
	require.Equal(t, "model:special", result.Pools[2].ID)
	for _, invalid := range []any{nil, "3", -1.0, math.NaN(), math.Inf(1)} {
		_, ok := parseRateLimitWindow("five_hour", map[string]any{"utilization": invalid}, now)
		require.False(t, ok)
	}
}

func TestParseRateLimitsIdentifiesOnlyKnownFableWeeklyDuration(t *testing.T) {
	t.Parallel()
	result := parseRateLimits(map[string]any{"model_scoped": []any{
		map[string]any{"display_name": "Fable", "utilization": 51.0},
		map[string]any{"display_name": "other", "utilization": 10.0},
	}}, time.Now())
	require.Len(t, result.Pools, 2)
	require.Equal(t, "model:Fable", result.Pools[0].ID)
	require.Equal(t, "usage", result.Pools[0].Windows[0].ID)
	require.Equal(t, int64(604800), result.Pools[0].Windows[0].DurationSeconds)
	require.Zero(t, result.Pools[1].Windows[0].DurationSeconds)
}
