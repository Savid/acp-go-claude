package claude

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuotaRelayForwardsOneRequestAndRejectsRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", ".99")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(upstream.Close)
	relay, err := newQuotaRelay(t.Context(), QuotaAccess{token: "fixture", endpoint: upstream.URL}, "claude-fable-5-1", true)
	require.NoError(t, err)
	t.Cleanup(relay.close)
	for i := range 4 {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, relay.baseURL+"/v1/messages?beta=true", bytes.NewBufferString(`{"model":"claude-fable-5-1","max_tokens":1,"tools":[]}`))
		require.NoError(t, requestErr)
		request.Header.Set("Authorization", "Bearer fixture")
		response, doErr := http.DefaultClient.Do(request)
		require.NoError(t, doErr)
		_, copyErr := io.Copy(io.Discard, response.Body)
		require.NoError(t, copyErr)
		require.NoError(t, response.Body.Close())
		if i == 0 {
			require.Equal(t, 503, response.StatusCode)
		} else {
			require.Equal(t, 403, response.StatusCode)
		}
	}
	require.Equal(t, int32(1), calls.Load())
	result := <-relay.result
	require.NoError(t, result.err)
	require.Equal(t, 99.0, result.result.Windows[0].Percent)
}

func TestQuotaRelayRejectsDifferentIdentityOrUnboundedRequests(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(upstream.Close)
	relay, err := newQuotaRelay(t.Context(), QuotaAccess{token: "fixture", endpoint: upstream.URL}, "claude-haiku-4-5", false)
	require.NoError(t, err)
	t.Cleanup(relay.close)
	for _, tc := range []struct{ token, body string }{
		{"wrong", `{"model":"claude-haiku-4-5","max_tokens":1}`},
		{"fixture", `{"model":"claude-fable-5-1","max_tokens":1}`},
		{"fixture", `{"model":"claude-haiku-4-5","max_tokens":2}`},
		{"fixture", `{"model":"claude-haiku-4-5","max_tokens":1,"tools":[{}]}`},
	} {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, relay.baseURL+"/v1/messages", bytes.NewBufferString(tc.body))
		require.NoError(t, requestErr)
		request.Header.Set("Authorization", "Bearer "+tc.token)
		response, doErr := http.DefaultClient.Do(request)
		require.NoError(t, doErr)
		require.NoError(t, response.Body.Close())
		require.Equal(t, 403, response.StatusCode)
	}
	require.Zero(t, calls.Load())
}

func TestQuotaForwardClassifiesFailuresWithoutFollowingRedirects(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 503, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.Header().Set("Location", "http://127.0.0.1:1")
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			result := forwardQuota(t.Context(), client, server.URL, http.Header{}, nil, false)
			require.Equal(t, status == 401, result.result.NotAuthenticated)
			require.Equal(t, status == 403 || status == 404, result.result.Unavailable)
			if status == 401 || status == 403 || status == 404 {
				require.NoError(t, result.err)
			} else {
				require.Error(t, result.err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := forwardQuota(ctx, http.DefaultClient, "http://127.0.0.1:1", nil, nil, false)
	require.Error(t, result.err)
}

func TestMain(m *testing.M) {
	if os.Getenv("ACP_GO_CLAUDE_TEST_QUOTA_CHILD") == "1" {
		model := ""
		for i, arg := range os.Args {
			if arg == "--model" && i+1 < len(os.Args) {
				model = os.Args[i+1]
			}
		}
		if os.Getenv("ACP_GO_CLAUDE_TEST_QUOTA_HANG") == "1" {
			select {}
		}
		for range 4 {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"tools":[]}`, model)
			req, err := http.NewRequest(http.MethodPost, os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages", strings.NewReader(body))
			if err != nil {
				os.Exit(2)
			}
			req.Header.Set("Authorization", "Bearer "+os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				os.Exit(3)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProbeQuotaUsesBoundedChildAndJoinsOnCancel(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.2")
		w.WriteHeader(200)
	}))
	t.Cleanup(upstream.Close)
	executable, err := os.Executable()
	require.NoError(t, err)
	env := []string{"ACP_GO_CLAUDE_TEST_QUOTA_CHILD=1", "CLAUDE_CODE_OAUTH_TOKEN=fixture"}
	result, err := ProbeQuota(t.Context(), QuotaAccess{token: "fixture", endpoint: upstream.URL, haiku: "claude-haiku-4-5"}, executable, env, t.TempDir(), false)
	require.NoError(t, err)
	require.Len(t, result.Windows, 1)
	require.Equal(t, int32(1), calls.Load())
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = ProbeQuota(ctx, QuotaAccess{token: "fixture", endpoint: upstream.URL, haiku: "claude-haiku-4-5"}, executable, append(env, "ACP_GO_CLAUDE_TEST_QUOTA_HANG=1"), t.TempDir(), false)
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load())
}
