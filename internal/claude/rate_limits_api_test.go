package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type rateLimitsAPITransport struct {
	*fakeTransport
	env map[string]string
}

func (t *rateLimitsAPITransport) LaunchEnvironment() map[string]string { return maps.Clone(t.env) }

func TestReadRateLimitsSetupTokenFallback(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			reset := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, "/proxy/v1/messages", r.URL.Path)
				require.Equal(t, "Bearer configured-setup-token", r.Header.Get("Authorization"))
				require.Equal(t, "oauth-2025-04-20", r.Header.Get("anthropic-beta"))
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, float64(1), body["max_tokens"])
				require.Equal(t, "configured-haiku", body["model"])
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.375")
				w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))
				w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "1.25")
				w.WriteHeader(status)
			}))
			defer server.Close()
			transport := &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{
				directAPIOAuthTokenEnv: "configured-setup-token", directAPIBaseURLEnv: server.URL + "/proxy",
				"ANTHROPIC_DEFAULT_HAIKU_MODEL": "configured-haiku",
			}}
			client := NewClient(nil, Options{}, transport)
			go autoRespondInitialize(transport.fakeTransport)
			startClientForTest(t, client)
			go respondToControlRequestWithResponse(transport.fakeTransport, "get_usage", map[string]any{"rate_limits_available": status == http.StatusOK})
			go func() {
				respondToControlRequestWithResponse(transport.fakeTransport, "get_settings", map[string]any{"effective": map[string]any{}})
				respondToControlRequestAfter(transport.fakeTransport, "get_settings", 3, map[string]any{"effective": map[string]any{}})
			}()
			result, err := client.ReadRateLimitsWithFallback(t.Context(), true)
			require.NoError(t, err)
			require.Equal(t, int32(1), requests.Load())
			require.Len(t, result.Pools, 1)
			require.Equal(t, "subscription", result.Pools[0].ID)
			require.Equal(t, []RateLimitWindow{
				{ID: "five_hour", UsedPercent: 37.5, ResetsAt: reset},
				{ID: "seven_day", UsedPercent: 125},
			}, result.Pools[0].Windows)
			require.WithinDuration(t, time.Now(), result.ObservedAt, time.Second)
		})
	}
}

func TestReadRateLimitsFallbackRetainsNativeWithEitherDirectAPISetting(t *testing.T) {
	t.Parallel()
	for _, direct := range []bool{false, true} {
		t.Run(strconv.FormatBool(direct), func(t *testing.T) {
			t.Parallel()
			transport := &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{directAPIOAuthTokenEnv: "test-token"}}
			client := NewClient(nil, Options{}, transport)
			go autoRespondInitialize(transport.fakeTransport)
			startClientForTest(t, client)
			payload := map[string]any{"rate_limits_available": true,
				"rate_limits": map[string]any{"five_hour": map[string]any{"utilization": 42.0}}}
			go respondToControlRequestWithResponse(transport.fakeTransport, "get_usage", payload)
			result, err := client.ReadRateLimitsWithFallback(t.Context(), direct)
			require.NoError(t, err)
			require.Len(t, transport.sentPayloads(), 2)
			require.Equal(t, 42.0, result.Pools[0].Windows[0].UsedPercent)
		})
	}
}

func TestReadRateLimitsFallbackOptOutClassifiesApplicableAcquisition(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		available bool
		settings  map[string]any
		disabled  bool
	}{
		{name: "native unsupported", settings: map[string]any{}, disabled: true},
		{name: "native empty", available: true, settings: map[string]any{}, disabled: true},
		{name: "native helper", settings: map[string]any{"apiKeyHelper": "configured-helper"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			t.Cleanup(server.Close)
			transport := &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{
				directAPIOAuthTokenEnv: "configured-token", directAPIBaseURLEnv: server.URL,
			}}
			client := NewClient(nil, Options{}, transport)
			go autoRespondInitialize(transport.fakeTransport)
			startClientForTest(t, client)
			go respondToControlRequestWithResponse(transport.fakeTransport, "get_usage", map[string]any{"rate_limits_available": test.available})
			go respondToControlRequestWithResponse(transport.fakeTransport, "get_settings", map[string]any{"effective": test.settings})
			result, err := client.ReadRateLimitsWithFallback(t.Context(), false)
			if test.disabled {
				require.ErrorIs(t, err, ErrRateLimitsDisabled)
			} else {
				require.NoError(t, err)
				require.True(t, result.Unsupported)
			}
			require.Empty(t, result.Pools)
			require.Zero(t, calls.Load())
		})
	}
}

func TestReadRateLimitsFallbackPreservesAuthenticationFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "private authentication failure")
	}))
	t.Cleanup(server.Close)
	transport := &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{
		directAPIOAuthTokenEnv: "configured-token", directAPIBaseURLEnv: server.URL,
	}}
	client := NewClient(nil, Options{}, transport)
	go autoRespondInitialize(transport.fakeTransport)
	startClientForTest(t, client)
	go respondToControlRequestWithResponse(transport.fakeTransport, "get_usage", map[string]any{"rate_limits_available": false})
	go respondToControlRequestWithResponse(transport.fakeTransport, "get_settings", map[string]any{"effective": map[string]any{}})
	result, err := client.ReadRateLimitsWithFallback(t.Context(), true)
	require.ErrorIs(t, err, ErrRateLimitsNotAuthenticated)
	require.NotContains(t, err.Error(), "private")
	require.Empty(t, result.Pools)
}

func TestReadRateLimitsSetupTokenFallbackDiscardsChangedSettings(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
	}))
	defer server.Close()
	transport := &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{
		directAPIOAuthTokenEnv: "configured-token", directAPIBaseURLEnv: server.URL,
	}}
	client := NewClient(nil, Options{}, transport)
	go autoRespondInitialize(transport.fakeTransport)
	startClientForTest(t, client)
	go respondToControlRequestWithResponse(transport.fakeTransport, "get_usage", map[string]any{"rate_limits_available": false})
	go func() {
		respondToControlRequestWithResponse(transport.fakeTransport, "get_settings", map[string]any{"effective": map[string]any{}})
		respondToControlRequestAfter(transport.fakeTransport, "get_settings", 3, map[string]any{"effective": map[string]any{"apiKeyHelper": "new-account-helper"}})
	}()
	result, err := client.ReadRateLimitsWithFallback(t.Context(), true)
	require.NoError(t, err)
	require.Empty(t, result.Pools)
	require.False(t, result.Unsupported)
}

func TestRateLimitsFallbackRejectsDifferentIdentity(t *testing.T) {
	t.Parallel()
	base := map[string]string{directAPIOAuthTokenEnv: "setup-token", directAPIBaseURLEnv: "https://same.example"}
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		env := maps.Clone(base)
		env[key] = "configured"
		_, ok := resolveRateLimitsAPIAccess(env)
		require.False(t, ok, key)
	}
	for _, settings := range []map[string]any{
		{"apiKeyHelper": "secret-helper"},
		{"forceLoginMethod": "gateway"},
		{"env": map[string]any{directAPIOAuthTokenEnv: "another-token"}},
		{"env": map[string]any{directAPIBaseURLEnv: "https://another.example"}},
		{"env": map[string]any{"ANTHROPIC_API_KEY": "another-key"}},
		{"env": map[string]any{directAPIOAuthTokenEnv: 42}},
	} {
		require.False(t, directAPISettingsMatch(settings, base))
	}
	for _, endpoint := range []string{"https://name:password@example.test", "file:///secret", "https://example.test?key=other", "https://example.test#fragment"} {
		env := maps.Clone(base)
		env[directAPIBaseURLEnv] = endpoint
		_, ok := resolveRateLimitsAPIAccess(env)
		require.False(t, ok, endpoint)
	}
	require.True(t, directAPISettingsMatch(map[string]any{"env": map[string]any{directAPIOAuthTokenEnv: "setup-token"}}, base))
}

func TestRateLimitsHeaderProbeRejectsRedirectsAndOversizeBodies(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	t.Cleanup(func() { require.False(t, leaked.Load()) })
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	t.Cleanup(target.Close)
	for _, mode := range []string{"redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)

					return
				}
				_, _ = fmt.Fprint(w, strings.Repeat("x", rateLimitsMaxBody+1))
			}))
			defer server.Close()
			_, err := readRateLimitsAPI(t.Context(), rateLimitsAPIAccess{endpoint: server.URL, token: "test-token", model: "test"})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "test-token")
		})
	}
}

func TestRateLimitsHeaderProbeHonorsCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readRateLimitsAPI(ctx, rateLimitsAPIAccess{endpoint: server.URL, token: "test-token", model: "test"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("quota probe ignored cancellation")
	}
}

func TestParseRateLimitsHeadersOmitsInvalidOptionalFacts(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for _, reset := range []string{"invalid", "9223372036854775807"} {
		headers := http.Header{}
		headers.Set("anthropic-ratelimit-unified-5h-utilization", "0")
		headers.Set("anthropic-ratelimit-unified-5h-reset", reset)
		result := parseRateLimitsHeaders(headers, now)
		require.Len(t, result.Pools, 1)
		require.True(t, result.Pools[0].Windows[0].ResetsAt.IsZero())
	}
	for _, fraction := range []string{"NaN", "+Inf", "-1", "1e308"} {
		headers := http.Header{}
		headers.Set("anthropic-ratelimit-unified-5h-utilization", fraction)
		require.Empty(t, parseRateLimitsHeaders(headers, now).Pools)
	}
}
