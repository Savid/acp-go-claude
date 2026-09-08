package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	rateLimitsSetupToken  = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	rateLimitsAPIKeyEnv   = "ANTHROPIC_API_KEY"       //nolint:gosec // Environment variable name, not a credential.
	rateLimitsBaseURL     = "ANTHROPIC_BASE_URL"
	rateLimitsDefaultBase = "https://api.anthropic.com"
	rateLimitsMaxBody     = 1 << 20
)

type rateLimitsAPIAccess struct {
	token      string
	endpoint   string
	model      string
	fableModel string
}

// ReadRateLimitsWithFallback prefers structured native observations. When they
// are absent, an effective setup token can obtain quota headers with one native
// Fable request, then one Haiku fallback, each max_tokens:1. It never discovers
// credentials from another home.
func (c *Client) ReadRateLimitsWithFallback(ctx context.Context, direct bool) (RateLimits, error) {
	nativeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, nativeErr := c.ReadRateLimits(nativeCtx)

	cancel()

	if len(result.Pools) > 0 || ctx.Err() != nil || c.activeController() == nil {
		return result, nativeErr
	}

	if errors.Is(nativeErr, c.options.ContainmentIncomplete) && nativeErr != nil {
		return result, nativeErr
	}

	if c.options.Authority != nil && c.options.Authority.Unavailable != nil && errors.Is(nativeErr, c.options.Authority.Unavailable) {
		return result, nativeErr
	}

	access, eligible := c.rateLimitsAPIAccess(ctx)
	if !eligible {
		return result, nativeErr
	}

	if !direct {
		return RateLimits{}, ErrRateLimitsDisabled
	}

	result, err := c.readRateLimitsSetupToken(ctx, access)
	if err != nil {
		return RateLimits{}, err
	}

	if len(result.Pools) == 0 {
		return result, nil
	}

	current, eligible := c.rateLimitsAPIAccess(ctx)
	if !eligible || current != access {
		return RateLimits{}, nil
	}

	return result, nil
}

func (c *Client) readRateLimitsSetupToken(ctx context.Context, access rateLimitsAPIAccess) (RateLimits, error) {
	if filepath.IsAbs(c.options.Cwd) || filepath.IsAbs(c.options.ScratchParent) {
		result, err := c.readRateLimitsNativeProbe(ctx, access)
		if rateLimitsContainmentError(c.options, err) || ctx.Err() != nil {
			return RateLimits{}, errors.Join(err, ctx.Err())
		}

		if errors.Is(err, ErrRateLimitsNotAuthenticated) {
			return RateLimits{}, err
		}

		if len(result.Pools) > 0 {
			return result, nil
		}

		current, eligible := c.rateLimitsAPIAccess(ctx)
		if !eligible || current != access {
			return RateLimits{}, nil
		}
	}

	return readRateLimitsAPI(ctx, access)
}

func (c *Client) rateLimitsAPIAccess(ctx context.Context) (rateLimitsAPIAccess, bool) {
	if c.options.Bare {
		return rateLimitsAPIAccess{}, false
	}

	source, ok := c.transport.(interface{ rateLimitsEnvironment() map[string]string })
	if !ok {
		return rateLimitsAPIAccess{}, false
	}

	env := source.rateLimitsEnvironment()
	if strings.TrimSpace(env[EnvironmentKey(rateLimitsSetupToken)]) == "" {
		return rateLimitsAPIAccess{}, false
	}

	settings, err := c.GetSettings(ctx)
	if err != nil || settings.Effective == nil || !rateLimitsSettingsMatch(settings.Effective, env) {
		return rateLimitsAPIAccess{}, false
	}

	return resolveRateLimitsAPIAccess(env)
}

// Effective settings are a disk/settings merge, not the process environment.
// Refuse differing credential or routing overrides rather than guessing which
// of them Claude applied; matching overrides preserve the captured identity.
func rateLimitsSettingsMatch(settings map[string]any, env map[string]string) bool {
	if helper, exists := settings["apiKeyHelper"]; exists && helper != "" {
		return false
	}

	if method, exists := settings["forceLoginMethod"]; exists && method == "gateway" {
		return false
	}

	raw, exists := settings["env"]
	if !exists {
		return true
	}

	values, ok := raw.(map[string]any)
	if !ok {
		return false
	}

	for key, rawValue := range values {
		value, valid := rawValue.(string)
		if !valid {
			return false
		}

		if rateLimitsIdentityEnv(key) && env[EnvironmentKey(key)] != value {
			return false
		}
	}

	return true
}

func rateLimitsIdentityEnv(key string) bool {
	key = strings.ToUpper(key)

	return strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") ||
		strings.HasPrefix(key, "AWS_") || strings.HasPrefix(key, "AZURE_") || strings.HasPrefix(key, "GOOGLE_")
}

func resolveRateLimitsAPIAccess(env map[string]string) (rateLimitsAPIAccess, bool) {
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", rateLimitsAPIKeyEnv, "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_AWS_API_KEY", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "ANTHROPIC_UNIX_SOCKET", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_BG_AUTH_SNAPSHOT_PATH", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR", "CLAUDE_CODE_WEBSOCKET_AUTH_TOKEN", "CLAUDE_SESSION_INGRESS_TOKEN_FILE"} {
		if strings.TrimSpace(env[EnvironmentKey(key)]) != "" {
			return rateLimitsAPIAccess{}, false
		}
	}

	token := strings.TrimSpace(env[EnvironmentKey(rateLimitsSetupToken)])
	if token == "" {
		return rateLimitsAPIAccess{}, false
	}

	base := strings.TrimSpace(env[EnvironmentKey(rateLimitsBaseURL)])
	if base == "" {
		base = rateLimitsDefaultBase
	}

	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return rateLimitsAPIAccess{}, false
	}

	if endpoint.Hostname() == "api.anthropic.com" && !strings.HasPrefix(token, "sk-ant-") {
		return rateLimitsAPIAccess{}, false
	}

	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/messages"
	model := "claude-haiku-4-5"

	for _, key := range []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
		if configured := strings.TrimSpace(env[EnvironmentKey(key)]); configured != "" {
			model = configured
		}
	}

	fable := strings.TrimSpace(env[EnvironmentKey("ANTHROPIC_DEFAULT_FABLE_MODEL")])
	if fable == "" {
		fable = "claude-fable-5-1"
	}

	return rateLimitsAPIAccess{token: token, endpoint: endpoint.String(), model: model, fableModel: fable}, true
}

func readRateLimitsAPI(ctx context.Context, access rateLimitsAPIAccess) (RateLimits, error) {
	body, err := json.Marshal(map[string]any{
		keyModel: access.model, "max_tokens": 1,
		"messages": []map[string]string{{"role": MessageTypeUser, keyContent: "quota"}},
	})
	if err != nil {
		return RateLimits{}, errors.New("encode quota probe")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, access.endpoint, bytes.NewReader(body))
	if err != nil {
		return RateLimits{}, errors.New("construct quota probe")
	}

	request.Header.Set("Authorization", "Bearer "+access.token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "acp-go-claude")

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	response, err := client.Do(request)
	if err != nil {
		return RateLimits{}, errors.New("quota probe failed")
	}
	defer response.Body.Close()

	observedAt := time.Now().UTC()

	if response.StatusCode == http.StatusUnauthorized {
		return RateLimits{}, ErrRateLimitsNotAuthenticated
	}

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusTooManyRequests {
		return RateLimits{}, errors.New("quota probe rejected")
	}

	read, err := io.Copy(io.Discard, io.LimitReader(response.Body, rateLimitsMaxBody+1))
	if err != nil || read > rateLimitsMaxBody {
		return RateLimits{}, errors.New("quota probe response unreadable")
	}

	return parseRateLimitsHeaders(response.Header, observedAt), nil
}

func parseRateLimitsHeaders(headers http.Header, observedAt time.Time) RateLimits {
	result := RateLimits{ObservedAt: observedAt}
	pool := RateLimitPool{ID: "subscription"}

	for _, window := range []struct{ id, prefix string }{
		{rateLimitFiveHour, "anthropic-ratelimit-unified-5h-"},
		{rateLimitSevenDay, "anthropic-ratelimit-unified-7d-"},
		{rateLimitUsage, "anthropic-ratelimit-unified-7d_oi-"},
	} {
		fraction, err := strconv.ParseFloat(headers.Get(window.prefix+"utilization"), 64)

		percent := fraction * 100
		if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 {
			continue
		}

		value := RateLimitWindow{ID: window.id, UsedPercent: percent}
		if reset, parseErr := strconv.ParseInt(headers.Get(window.prefix+"reset"), 10, 64); parseErr == nil {
			parsed := time.Unix(reset, 0).UTC()
			if parsed.Year() >= 0 && parsed.Year() <= 9999 {
				if !parsed.After(observedAt) {
					continue
				}

				value.ResetsAt = parsed
			}
		}

		if window.id == rateLimitUsage {
			value.DurationSeconds = rateLimitFableSeconds
			result.Pools = append(result.Pools, RateLimitPool{ID: "model:" + rateLimitFableLabel, Label: rateLimitFableLabel, Windows: []RateLimitWindow{value}})
		} else {
			pool.Windows = append(pool.Windows, value)
		}
	}

	if len(pool.Windows) > 0 {
		result.Pools = append([]RateLimitPool{pool}, result.Pools...)
	}

	return result
}
