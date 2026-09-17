package claude

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuotaHeadersRequireFableProbeForFableWindow(t *testing.T) {
	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.23")
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "1.12")
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.99")
	haiku := quotaHeaders(headers, false, now)
	require.Len(t, haiku, 2)
	require.InDelta(t, 112, haiku[1].Percent, .0001)
	fable := quotaHeaders(headers, true, now)
	require.Len(t, fable, 3)
	require.Equal(t, QuotaFable, fable[2].ID)
	require.Equal(t, now.Add(30*time.Minute), fable[2].StaleAt)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "NaN")
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "-1")
	require.Empty(t, quotaHeaders(headers, false, now))
}

func TestQuotaEventsPreserveScopeAndDoNotInventPercentages(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, want string
		fable      bool
	}{
		{"five_hour", QuotaSession, true}, {"seven_day", QuotaWeekly, true}, {"seven_day_overage_included", QuotaFable, true}, {"seven_day_overage_included", "unknown", false},
	} {
		raw, _ := json.Marshal(map[string]any{"rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": tc.name}})
		windows, exhausted := QuotaEvent(Event{Type: "rate_limit_event", Raw: raw}, tc.fable, now)
		require.Empty(t, windows)
		require.Equal(t, tc.want, exhausted)
	}
	raw := json.RawMessage(`{"rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1800000000},"seven_day":{"utilization":0.8,"resetsAt":1800000000}}}}`)
	windows, exhausted := QuotaEvent(Event{Type: "rate_limit_event", Raw: raw}, true, now)
	require.Len(t, windows, 2)
	require.Equal(t, QuotaSession, exhausted)
}
