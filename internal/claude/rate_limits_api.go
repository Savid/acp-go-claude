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
	rateLimitsMaxBody           = 1 << 20
	rateLimitsDefaultFableModel = "claude-fable-5-1"
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

	source, ok := c.transport.(interface{ LaunchEnvironment() map[string]string })
	if !ok {
		return rateLimitsAPIAccess{}, false
	}

	env := source.LaunchEnvironment()
	if strings.TrimSpace(env[EnvironmentKey(directAPIOAuthTokenEnv)]) == "" {
		return rateLimitsAPIAccess{}, false
	}

	settings, err := c.GetSettings(ctx)
	if err != nil || settings.Effective == nil || !directAPISettingsMatch(settings.Effective, env) {
		return rateLimitsAPIAccess{}, false
	}

	return resolveRateLimitsAPIAccess(env)
}

func resolveRateLimitsAPIAccess(env map[string]string) (rateLimitsAPIAccess, bool) {
	if directAPIHasRouteOverride(env) || strings.TrimSpace(env[EnvironmentKey(directAPIKeyEnv)]) != "" {
		return rateLimitsAPIAccess{}, false
	}

	token := strings.TrimSpace(env[EnvironmentKey(directAPIOAuthTokenEnv)])
	if token == "" {
		return rateLimitsAPIAccess{}, false
	}

	base := strings.TrimSpace(env[EnvironmentKey(directAPIBaseURLEnv)])
	if base == "" {
		base = directAPIDefaultBase
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
		fable = rateLimitsDefaultFableModel
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
