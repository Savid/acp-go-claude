package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// isolateEnv makes BuildEnv see exactly env, so an ambient ANTHROPIC_* variable
// on the developer's machine cannot steer a probe test.
func isolateEnv(t *testing.T, env []string) {
	t.Helper()

	original := commandEnviron
	t.Cleanup(func() { commandEnviron = original })

	commandEnviron = func() []string { return env }
}

// routeHTTP points the probe at server. Only the transport is swapped, so the
// client's real redirect policy stays under test.
func routeHTTP(t *testing.T, server *httptest.Server) {
	t.Helper()

	original := usageHTTPClient.Transport
	t.Cleanup(func() { usageHTTPClient.Transport = original })

	usageHTTPClient.Transport = server.Client().Transport
}

func TestResolveAPIAccess(t *testing.T) {
	require.Equal(t, map[string]string{"A": "1"}, environMap([]string{"bad", "=empty", "A=1"}))

	tests := []struct {
		name    string
		env     map[string]string
		wantURL string
		wantTok string
		wantOK  bool
	}{
		{
			name:   "no token",
			env:    map[string]string{},
			wantOK: false,
		},
		{
			name:    "oauth token wins over auth token",
			env:     map[string]string{envClaudeCodeOAuthToken: "sk-ant-oat01-a", envAnthropicAuthToken: "sk-ant-oat01-b"},
			wantURL: defaultAnthropicBaseURL,
			wantTok: "sk-ant-oat01-a",
			wantOK:  true,
		},
		{
			name:    "auth token used when oauth token absent",
			env:     map[string]string{envAnthropicAuthToken: "sk-ant-oat01-b"},
			wantURL: defaultAnthropicBaseURL,
			wantTok: "sk-ant-oat01-b",
			wantOK:  true,
		},
		{
			name:   "non-anthropic token is never sent to anthropic",
			env:    map[string]string{envAnthropicAuthToken: "gateway-token"},
			wantOK: false,
		},
		{
			name:    "explicit base url accepts any token",
			env:     map[string]string{envAnthropicAuthToken: "gateway-token", envAnthropicBaseURL: "https://gw.example/"},
			wantURL: "https://gw.example",
			wantTok: "gateway-token",
			wantOK:  true,
		},
		{
			name:   "base url carrying userinfo is refused",
			env:    map[string]string{envAnthropicAuthToken: "sk-ant-oat01-x", envAnthropicBaseURL: "https://user:pass@gw.example"},
			wantOK: false,
		},
		{
			// An unparseable base URL is left for the request builder to report.
			name:    "unparseable base url is passed through",
			env:     map[string]string{envAnthropicAuthToken: "sk-ant-oat01-x", envAnthropicBaseURL: "://nope"},
			wantURL: "://nope",
			wantTok: "sk-ant-oat01-x",
			wantOK:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseURL, token, ok := resolveAPIAccess(test.env)

			require.Equal(t, test.wantOK, ok)
			require.Equal(t, test.wantURL, baseURL)
			require.Equal(t, test.wantTok, token)
		})
	}
}

func TestUsageEndpointWindowID(t *testing.T) {
	tests := []struct {
		kind  string
		model string
		want  string
	}{
		{kind: windowSession, want: windowSession},
		{kind: kindWeeklyAll, want: windowWeekAllModels},
		{kind: kindWeeklyScoped, model: "Fable", want: "week-fable"},
		{kind: kindWeeklyScoped, model: "Claude Opus", want: "week-claude-opus"},
		{kind: kindWeeklyScoped, want: ""},
		{kind: "tangelo", want: ""},
	}

	for _, test := range tests {
		require.Equal(t, test.want, usageEndpointWindowID(test.kind, test.model))
	}
}

func TestProbeModel(t *testing.T) {
	require.Equal(t, defaultRateLimitsProbeModel, probeModel(map[string]string{}))
	require.Equal(t, "small", probeModel(map[string]string{envAnthropicSmallModel: "small"}))
	require.Equal(t, "haiku", probeModel(map[string]string{envAnthropicHaikuModel: "haiku"}))
	require.Equal(
		t,
		"small",
		probeModel(map[string]string{envAnthropicSmallModel: "small", envAnthropicHaikuModel: "haiku"}),
	)
}

func TestParseUnifiedHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-5h-utilization", "0.06")
	header.Set("anthropic-ratelimit-unified-5h-reset", "1783624800")
	header.Set("anthropic-ratelimit-unified-7d-utilization", "0.99")

	windows := parseUnifiedHeaders(header)
	require.Len(t, windows, 2)

	require.Equal(t, "session", windows[0].ID)
	require.InDelta(t, 6.0, windows[0].UsedPercent, 0.0001)
	require.Equal(t, time.Unix(1783624800, 0).UTC(), windows[0].ResetsAt)

	// A window without a reset header still reports its utilization.
	require.Equal(t, "week-all-models", windows[1].ID)
	require.InDelta(t, 99.0, windows[1].UsedPercent, 0.0001)
	require.True(t, windows[1].ResetsAt.IsZero())
}

func TestParseUnifiedHeadersSkipsUnparseableUtilization(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-5h-utilization", "not-a-number")

	require.Empty(t, parseUnifiedHeaders(header))
}

const usageEndpointSample = `{"limits":[
  {"kind":"session","percent":6,"resets_at":"2026-07-09T16:59:59Z"},
  {"kind":"weekly_all","percent":97,"resets_at":"2026-07-10T04:59:59Z"},
  {"kind":"weekly_scoped","percent":100,"resets_at":"2026-07-10T04:59:59Z",
   "scope":{"model":{"display_name":"Fable"}}},
  {"kind":"tangelo","percent":1}
]}`

func TestQueryRateLimitsAPIUsesUsageEndpoint(t *testing.T) {
	var gotAuth, gotBeta, gotAgent string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/oauth/usage", request.URL.Path)

		gotAuth = request.Header.Get("Authorization")
		gotBeta = request.Header.Get("anthropic-beta")
		gotAgent = request.Header.Get("User-Agent")

		_, _ = writer.Write([]byte(usageEndpointSample))
	}))
	defer server.Close()

	isolateEnv(t, []string{envAnthropicBaseURL + "=" + server.URL, envAnthropicAuthToken + "=sk-ant-oat01-x"})
	routeHTTP(t, server)

	limits, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{UserAgent: "acp-go-claude/1.2.3"})
	require.NoError(t, err)

	require.Equal(t, "Bearer sk-ant-oat01-x", gotAuth)
	require.Equal(t, anthropicOAuthBeta, gotBeta)
	require.Equal(t, "acp-go-claude/1.2.3", gotAgent)

	require.Len(t, limits.Windows, 3)
	require.Equal(t, "session", limits.Windows[0].ID)
	require.InDelta(t, 6.0, limits.Windows[0].UsedPercent, 0.0001)
	require.Equal(t, "2026-07-09T16:59:59Z", limits.Windows[0].ResetsAt.Format(time.RFC3339))
	require.Equal(t, "week-all-models", limits.Windows[1].ID)
	require.Equal(t, "week-fable", limits.Windows[2].ID)
	require.InDelta(t, 100.0, limits.Windows[2].UsedPercent, 0.0001)
}

// An inference-only token gets 403 from the usage endpoint; the unified
// rate-limit headers on a one-token probe are the only surface left.
func TestQueryRateLimitsAPIFallsBackToHeaderProbe(t *testing.T) {
	var probedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/oauth/usage" {
			writer.WriteHeader(http.StatusForbidden)

			return
		}

		require.Equal(t, "/v1/messages", request.URL.Path)

		var readErr error

		probedBody, readErr = io.ReadAll(request.Body)
		require.NoError(t, readErr)

		writer.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.0")
		writer.Header().Set("anthropic-ratelimit-unified-5h-reset", "1783624800")
		writer.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.99")
		writer.Header().Set("anthropic-ratelimit-unified-7d-reset", "1783756800")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	isolateEnv(t, []string{
		envAnthropicBaseURL + "=" + server.URL,
		envClaudeCodeOAuthToken + "=sk-ant-oat01-x",
		envAnthropicSmallModel + "=claude-haiku-4-5",
	})
	routeHTTP(t, server)

	limits, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.NoError(t, err)

	require.Contains(t, string(probedBody), `"model":"claude-haiku-4-5"`)
	require.Contains(t, string(probedBody), `"max_tokens":1`)

	require.Len(t, limits.Windows, 2)
	require.Equal(t, "session", limits.Windows[0].ID)
	require.InDelta(t, 0.0, limits.Windows[0].UsedPercent, 0.0001)
	require.Equal(t, "week-all-models", limits.Windows[1].ID)
	require.InDelta(t, 99.0, limits.Windows[1].UsedPercent, 0.0001)
}

func TestQueryRateLimitsAPIWithoutTokenReturnsEmpty(t *testing.T) {
	isolateEnv(t, nil)

	limits, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.NoError(t, err)
	require.Empty(t, limits.Windows)
}

// A usage endpoint that answers 200 with garbage still yields to the header
// probe rather than failing the call.
func TestQueryRateLimitsAPIFallsBackWhenUsageEndpointIsUndecodable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/oauth/usage" {
			_, _ = writer.Write([]byte(`{"limits":`))

			return
		}

		writer.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	// A malformed environ entry is skipped rather than parsed as a variable.
	isolateEnv(t, []string{
		"MALFORMED",
		"=novalue",
		envAnthropicBaseURL + "=" + server.URL,
		envAnthropicAuthToken + "=sk-ant-oat01-x",
	})
	routeHTTP(t, server)

	limits, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.NoError(t, err)
	require.Len(t, limits.Windows, 1)
	require.InDelta(t, 50.0, limits.Windows[0].UsedPercent, 0.0001)
}

// A redirect is never followed, so the bearer token cannot be carried to a host
// or scheme the caller never configured. Both probes see the 3xx as a failure.
func TestQueryRateLimitsAPINeverFollowsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://evil.example"+request.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	isolateEnv(t, []string{envAnthropicBaseURL + "=" + server.URL, envAnthropicAuthToken + "=sk-ant-oat01-x"})
	routeHTTP(t, server)

	_, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.ErrorContains(t, err, "usage endpoint returned 302")
	require.ErrorContains(t, err, "usage probe returned 302")
}

// An unreachable base URL fails both probes at the transport.
func TestQueryRateLimitsAPIReportsTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	routeHTTP(t, server)
	server.Close()

	isolateEnv(t, []string{envAnthropicBaseURL + "=" + url, envAnthropicAuthToken + "=sk-ant-oat01-x"})

	_, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.ErrorContains(t, err, "query usage endpoint")
	require.ErrorContains(t, err, "run usage probe")
}

// A base URL that cannot form a request fails before any call is made.
func TestQueryRateLimitsAPIReportsRequestBuildFailures(t *testing.T) {
	isolateEnv(t, []string{envAnthropicBaseURL + "=://nope", envAnthropicAuthToken + "=sk-ant-oat01-x"})

	_, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.ErrorContains(t, err, "build usage request")
}

func TestQueryRateLimitsAPIReportsBothProbeFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	isolateEnv(t, []string{envAnthropicBaseURL + "=" + server.URL, envAnthropicAuthToken + "=sk-ant-oat01-x"})
	routeHTTP(t, server)

	_, err := QueryRateLimitsAPI(context.Background(), RateLimitsProbe{})
	require.ErrorContains(t, err, "usage endpoint returned 500")
	require.ErrorContains(t, err, "usage probe returned 500")
}
