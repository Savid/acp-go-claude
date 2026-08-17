package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RateLimitsProbe configures a direct Anthropic API usage probe. Options
// supplies the same environment the Claude CLI would launch with, so the probe
// resolves the base URL and bearer token exactly as the harness does.
type RateLimitsProbe struct {
	Options   Options
	UserAgent string
}

const (
	defaultAnthropicBaseURL = "https://api.anthropic.com"

	envAnthropicBaseURL         = "ANTHROPIC_BASE_URL"
	envAnthropicAuthToken       = "ANTHROPIC_AUTH_TOKEN"    // #nosec G101 -- env var name, not a secret.
	envClaudeCodeOAuthToken     = "CLAUDE_CODE_OAUTH_TOKEN" // #nosec G101 -- env var name, not a secret.
	envAnthropicSmallModel      = "ANTHROPIC_SMALL_FAST_MODEL"
	envAnthropicHaikuModel      = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
	defaultRateLimitsProbeModel = "claude-haiku-4-5"

	anthropicOAuthBeta = "oauth-2025-04-20"
	anthropicVersion   = "2023-06-01"

	// maxUsageResponseBytes caps the usage payload the decoder will read, so a
	// faulty endpoint cannot stream unbounded JSON into memory.
	maxUsageResponseBytes = 1 << 20

	// anthropicTokenPrefix marks a token as Anthropic-issued. Without an
	// explicit ANTHROPIC_BASE_URL the probe refuses to send anything else to
	// api.anthropic.com, so a gateway token never leaks to Anthropic.
	anthropicTokenPrefix = "sk-ant-" // #nosec G101 -- token prefix, not a secret.

	windowSession       = "session"
	windowWeekAllModels = "week-all-models"

	// Usage-endpoint window kinds. "session" doubles as its window id.
	kindWeeklyAll    = "weekly_all"
	kindWeeklyScoped = "weekly_scoped"
)

// usageHTTPClient issues the direct API probes. Redirects are never followed:
// the bearer token rides in a header, and a redirect could carry it to a host
// or scheme the caller never configured. A 3xx is surfaced as a failed probe.
var usageHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// QueryRateLimitsAPI reads subscription usage straight from the Anthropic API.
// It prefers the OAuth usage endpoint, which reports every window, and falls
// back to the unified rate-limit response headers on a one-token inference
// request when the token lacks the user:profile scope.
//
// It returns empty windows and no error when no Anthropic bearer token is
// configured: the caller has nothing to ask about.
func QueryRateLimitsAPI(ctx context.Context, probe RateLimitsProbe) (RateLimits, error) {
	env := environMap(BuildEnv(probe.Options))

	baseURL, token, ok := resolveAPIAccess(env)
	if !ok {
		return RateLimits{}, nil
	}

	limits, err := queryUsageEndpoint(ctx, baseURL, token, probe.userAgent())
	if err == nil {
		return limits, nil
	}

	// Long-lived tokens (`claude setup-token`, CLAUDE_CODE_OAUTH_TOKEN) are
	// inference-only by design, so the usage endpoint is closed to them. The
	// unified rate-limit headers on an inference request are the only surface
	// left, and they carry the session and all-models weekly windows.
	headerLimits, headerErr := queryUsageHeaders(ctx, baseURL, token, probe.userAgent(), env)
	if headerErr != nil {
		return RateLimits{}, fmt.Errorf("%w (header probe: %w)", err, headerErr)
	}

	return headerLimits, nil
}

// resolveAPIAccess picks the base URL and bearer token the Claude CLI would
// use. An Anthropic-issued token is required when the base URL is the default,
// so a gateway token bound to ANTHROPIC_AUTH_TOKEN is never sent to Anthropic.
func resolveAPIAccess(env map[string]string) (string, string, bool) {
	token := env[envClaudeCodeOAuthToken]
	if token == "" {
		token = env[envAnthropicAuthToken]
	}

	if token == "" {
		return "", "", false
	}

	baseURL := strings.TrimSuffix(strings.TrimSpace(env[envAnthropicBaseURL]), "/")
	if baseURL == "" {
		if !strings.HasPrefix(token, anthropicTokenPrefix) {
			return "", "", false
		}

		baseURL = defaultAnthropicBaseURL
	}

	// A base URL carrying userinfo would embed its credentials in every
	// transport error, and those errors reach the debug log. Refuse it; an
	// unparsable URL is left for the request builder to report.
	if parsed, err := url.Parse(baseURL); err == nil && parsed.User != nil {
		return "", "", false
	}

	return baseURL, token, true
}

func environMap(env []string) map[string]string {
	values := make(map[string]string, len(env))

	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}

		values[key] = value
	}

	return values
}

func (p RateLimitsProbe) userAgent() string {
	if p.UserAgent == "" {
		return "acp-go-claude"
	}

	return p.UserAgent
}

func newAPIRequest(
	ctx context.Context,
	method string,
	endpoint string,
	token string,
	userAgent string,
	body string,
) (*http.Request, error) {
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("build usage request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", anthropicOAuthBeta)
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("Content-Type", "application/json")

	if userAgent != "" {
		request.Header.Set("User-Agent", userAgent)
	}

	return request, nil
}

// usageEndpointResponse mirrors the `limits` array of GET /api/oauth/usage,
// the same source that backs the CLI's `/usage` panel.
type usageEndpointResponse struct {
	Limits []struct {
		Kind     string `json:"kind"`
		Percent  float64
		ResetsAt string `json:"resets_at"` //nolint:tagliatelle // Anthropic wire format.
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"` //nolint:tagliatelle // Anthropic wire format.
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

func queryUsageEndpoint(ctx context.Context, baseURL string, token string, userAgent string) (RateLimits, error) {
	request, err := newAPIRequest(ctx, http.MethodGet, baseURL+"/api/oauth/usage", token, userAgent, "")
	if err != nil {
		return RateLimits{}, err
	}

	response, err := usageHTTPClient.Do(request)
	if err != nil {
		return RateLimits{}, fmt.Errorf("query usage endpoint: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return RateLimits{}, fmt.Errorf("usage endpoint returned %d", response.StatusCode)
	}

	var payload usageEndpointResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxUsageResponseBytes)).Decode(&payload); err != nil {
		return RateLimits{}, fmt.Errorf("decode usage endpoint: %w", err)
	}

	windows := make([]RateLimitWindow, 0, len(payload.Limits))

	for _, limit := range payload.Limits {
		id := usageEndpointWindowID(limit.Kind, scopeModelName(limit.Scope))
		if id == "" {
			continue
		}

		window := RateLimitWindow{ID: id, UsedPercent: limit.Percent}
		if resetsAt, err := time.Parse(time.RFC3339, limit.ResetsAt); err == nil {
			window.ResetsAt = resetsAt
		}

		windows = append(windows, window)
	}

	return RateLimits{Windows: windows}, nil
}

func scopeModelName(scope *struct {
	Model *struct {
		DisplayName string `json:"display_name"` //nolint:tagliatelle // Anthropic wire format.
	} `json:"model"`
},
) string {
	if scope == nil || scope.Model == nil {
		return ""
	}

	return scope.Model.DisplayName
}

// usageEndpointWindowID maps an endpoint window onto the same vendor-native ids
// the `/usage` panel parse produces, so the wire shape never depends on which
// source answered. The "session" endpoint kind and window id coincide.
func usageEndpointWindowID(kind string, modelName string) string {
	switch kind {
	case windowSession:
		return windowSession
	case kindWeeklyAll:
		return windowWeekAllModels
	case kindWeeklyScoped:
		if modelName == "" {
			return ""
		}

		return "week-" + usageWindowID(modelName)
	default:
		return ""
	}
}

// unifiedHeaderWindows names the unified rate-limit header abbreviations and the
// window ids they map onto.
var unifiedHeaderWindows = []struct {
	id     string
	abbrev string
}{
	{id: windowSession, abbrev: "5h"},
	{id: windowWeekAllModels, abbrev: "7d"},
}

// queryUsageHeaders reads the unified rate-limit headers off a one-token
// inference request. This is a billable call against the account's quota; it is
// the only usage surface open to an inference-only token.
func queryUsageHeaders(
	ctx context.Context,
	baseURL string,
	token string,
	userAgent string,
	env map[string]string,
) (RateLimits, error) {
	// The only variable is the model name, so quoting it produces valid JSON
	// without an encoder whose error branch could never fire.
	body := fmt.Sprintf(
		`{"model":%s,"max_tokens":1,"messages":[{"role":%q,"content":"quota"}]}`,
		strconv.Quote(probeModel(env)),
		MessageTypeUser,
	)

	request, err := newAPIRequest(ctx, http.MethodPost, baseURL+"/v1/messages", token, userAgent, body)
	if err != nil {
		return RateLimits{}, err
	}

	response, err := usageHTTPClient.Do(request)
	if err != nil {
		return RateLimits{}, fmt.Errorf("run usage probe: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return RateLimits{}, fmt.Errorf("usage probe returned %d", response.StatusCode)
	}

	return RateLimits{Windows: parseUnifiedHeaders(response.Header)}, nil
}

func parseUnifiedHeaders(header http.Header) []RateLimitWindow {
	windows := make([]RateLimitWindow, 0, len(unifiedHeaderWindows))

	for _, window := range unifiedHeaderWindows {
		prefix := "anthropic-ratelimit-unified-" + window.abbrev

		utilization, err := strconv.ParseFloat(header.Get(prefix+"-utilization"), 64)
		if err != nil {
			continue
		}

		limit := RateLimitWindow{ID: window.id, UsedPercent: utilization * 100}

		if reset, err := strconv.ParseInt(header.Get(prefix+"-reset"), 10, 64); err == nil {
			limit.ResetsAt = time.Unix(reset, 0).UTC()
		}

		windows = append(windows, limit)
	}

	return windows
}

// probeModel mirrors the Claude CLI's own quota-check model resolution so the
// probe bills the cheapest model the caller has configured.
func probeModel(env map[string]string) string {
	if model := env[envAnthropicSmallModel]; model != "" {
		return model
	}

	if model := env[envAnthropicHaikuModel]; model != "" {
		return model
	}

	return defaultRateLimitsProbeModel
}
