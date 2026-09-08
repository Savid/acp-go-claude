package claude

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimitsBridgeForwardsOnlyOneBoundedNativeRequest(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, "/configured/v1/messages?beta=true", r.URL.RequestURI())
				require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				require.Equal(t, "native-beta", r.Header.Get("anthropic-beta"))
				require.Equal(t, "native-user-agent", r.Header.Get("User-Agent"))
				require.Empty(t, r.Header.Get("Cookie"))
				require.Empty(t, r.Header.Get("X-Unrelated"))
				close(entered)
				<-release
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
				w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.625")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "upstream-content-must-stay-private")
			}))
			t.Cleanup(upstream.Close)
			bridge, err := newRateLimitsBridge(t.Context(), rateLimitsAPIAccess{
				endpoint: upstream.URL + "/configured/v1/messages", token: "fixture-token", fableModel: "fixture-fable",
			})
			require.NoError(t, err)
			t.Cleanup(bridge.close)
			responseBody := make(chan string, 1)
			go func() {
				response := sendRateLimitsBridgeRequest(t, bridge, "/v1/messages?beta=true", "fixture-token", `{"model":"fixture-fable","max_tokens":1,"tools":[]}`)
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				require.NoError(t, readErr)
				require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
				responseBody <- string(body)
			}()
			<-entered
			second := sendRateLimitsBridgeRequest(t, bridge, "/v1/messages", "fixture-token", `{"model":"fixture-fable","max_tokens":1}`)
			require.Equal(t, http.StatusConflict, second.StatusCode)
			require.NoError(t, second.Body.Close())
			close(release)
			result := <-bridge.result
			require.NoError(t, result.err)
			require.Equal(t, int32(1), calls.Load())
			require.Len(t, result.value.Pools, 2)
			require.Equal(t, "subscription", result.value.Pools[0].ID)
			require.Equal(t, 25.0, result.value.Pools[0].Windows[0].UsedPercent)
			require.Equal(t, RateLimitPool{ID: "model:Fable", Label: "Fable", Windows: []RateLimitWindow{{ID: "usage", UsedPercent: 62.5, DurationSeconds: 604800}}}, result.value.Pools[1])
			require.NotContains(t, <-responseBody, "upstream-content")
		})
	}
}

func TestRateLimitsBridgePreservesAuthenticationFailure(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "private authentication failure")
	}))
	t.Cleanup(upstream.Close)
	bridge, err := newRateLimitsBridge(t.Context(), rateLimitsAPIAccess{
		endpoint: upstream.URL, token: "fixture-token", fableModel: "fixture-fable",
	})
	require.NoError(t, err)
	t.Cleanup(bridge.close)
	response := sendRateLimitsBridgeRequest(t, bridge, "/v1/messages", "fixture-token", `{"model":"fixture-fable","max_tokens":1}`)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.NotContains(t, string(body), "private")
	result := <-bridge.result
	require.ErrorIs(t, result.err, ErrRateLimitsNotAuthenticated)
	require.Empty(t, result.value.Pools)
}

func TestRateLimitsBridgeRejectsWrongRouteCredentialAndWork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(upstream.Close)
	bridge, err := newRateLimitsBridge(t.Context(), rateLimitsAPIAccess{endpoint: upstream.URL, token: "fixture-token", fableModel: "fixture-fable"})
	require.NoError(t, err)
	t.Cleanup(bridge.close)
	for _, test := range []struct{ route, token, body string }{
		{"/another", "fixture-token", `{"model":"fixture-fable","max_tokens":1}`},
		{"/v1/messages?other=true", "fixture-token", `{"model":"fixture-fable","max_tokens":1}`},
		{"/v1/messages", "different-token", `{"model":"fixture-fable","max_tokens":1}`},
		{"/v1/messages", "fixture-token", `{"model":"another","max_tokens":1}`},
		{"/v1/messages", "fixture-token", `{"model":"fixture-fable","max_tokens":2}`},
		{"/v1/messages", "fixture-token", `{"model":"fixture-fable","max_tokens":1,"tools":[{}]}`},
		{"/v1/messages", "fixture-token", strings.Repeat("x", rateLimitsProbeMaxRequest+1)},
	} {
		response := sendRateLimitsBridgeRequest(t, bridge, test.route, test.token, test.body)
		require.GreaterOrEqual(t, response.StatusCode, 400)
		require.NoError(t, response.Body.Close())
	}
	require.Zero(t, calls.Load())
}

func TestRateLimitsBridgeRejectsRedirectsAndOversizeBodies(t *testing.T) {
	t.Parallel()
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	t.Cleanup(target.Close)
	for _, mode := range []string{"redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)

					return
				}
				_, _ = fmt.Fprint(w, strings.Repeat("x", rateLimitsMaxBody+1))
			}))
			t.Cleanup(upstream.Close)
			bridge, err := newRateLimitsBridge(t.Context(), rateLimitsAPIAccess{endpoint: upstream.URL, token: "fixture-token", fableModel: "fixture-fable"})
			require.NoError(t, err)
			t.Cleanup(bridge.close)
			response := sendRateLimitsBridgeRequest(t, bridge, "/v1/messages", "fixture-token", `{"model":"fixture-fable","max_tokens":1}`)
			require.NoError(t, response.Body.Close())
			result := <-bridge.result
			require.Error(t, result.err)
			require.Empty(t, result.value.Pools)
			require.False(t, redirected.Load())
		})
	}
}

func TestRateLimitsBridgeCloseWaitsForCanceledForward(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	bridge, err := newRateLimitsBridge(ctx, rateLimitsAPIAccess{endpoint: upstream.URL, token: "fixture-token", fableModel: "fixture-fable"})
	require.NoError(t, err)
	t.Cleanup(bridge.close)
	var caller sync.WaitGroup
	caller.Go(func() {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, bridge.baseURL+"/v1/messages", strings.NewReader(`{"model":"fixture-fable","max_tokens":1}`))
		require.NoError(t, requestErr)
		request.Header.Set("Authorization", "Bearer fixture-token")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
		}
	})
	<-entered
	cancel()
	done := make(chan struct{})
	go func() { bridge.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge close did not settle its canceled request")
	}
	caller.Wait()
	require.Error(t, (<-bridge.result).err)
}

func sendRateLimitsBridgeRequest(t *testing.T, bridge *rateLimitsBridge, route, token, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, bridge.baseURL+route, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "native-beta")
	request.Header.Set("User-Agent", "native-user-agent")
	request.Header.Set("Cookie", "unrelated-cookie")
	request.Header.Set("X-Unrelated", "unrelated-header")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	return response
}
