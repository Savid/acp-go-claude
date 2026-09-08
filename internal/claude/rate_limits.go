package claude

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	rateLimitFiveHour           = "five_hour"
	rateLimitSevenDay           = "seven_day"
	rateLimitFableSeconds int64 = 7 * 24 * 60 * 60
	rateLimitFableLabel         = "Fable"
	rateLimitUsage              = "usage"
)

var (
	// ErrRateLimitsDisabled means the applicable setup-token acquisition is disabled.
	ErrRateLimitsDisabled = errors.New("quota probe disabled")
	// ErrRateLimitsNotAuthenticated means the effective credential was rejected.
	ErrRateLimitsNotAuthenticated = errors.New("quota probe not authenticated")
)

// RateLimits contains only observations from a structured usage read.
type RateLimits struct {
	Unsupported bool
	ObservedAt  time.Time
	PlanType    string
	Pools       []RateLimitPool
}

// RateLimitPool preserves the source allowance containing these windows.
type RateLimitPool struct {
	ID      string
	Label   string
	Windows []RateLimitWindow
}

// RateLimitWindow holds source percentage points and an optional reset.
type RateLimitWindow struct {
	ID              string
	UsedPercent     float64
	ResetsAt        time.Time
	DurationSeconds int64
}

// ReadRateLimits uses Claude's experimental get_usage control request without
// a user message or any usage-triggered behaviors.
func (c *Client) ReadRateLimits(ctx context.Context) (RateLimits, error) {
	controller := c.activeController()
	if controller == nil {
		return RateLimits{}, ErrClientNotStarted
	}

	resp, err := controller.SendRequest(ctx, "get_usage", map[string]any{"skip_behaviors": true}, 30*time.Second)
	if err != nil {
		return RateLimits{}, fmt.Errorf("get usage: %w", err)
	}

	payload, ok := resp.Response[keyResponse].(map[string]any)
	if !ok {
		return RateLimits{}, errors.New("get usage returned no structured response")
	}

	available, ok := payload["rate_limits_available"].(bool)
	if !ok {
		return RateLimits{}, errors.New("get usage omitted availability")
	}

	if !available {
		return RateLimits{Unsupported: true}, nil
	}

	limits, _ := payload["rate_limits"].(map[string]any)
	result := parseRateLimits(limits, time.Now().UTC())
	result.PlanType, _ = payload["subscription_type"].(string)

	return result, nil
}

func parseRateLimits(payload map[string]any, observedAt time.Time) RateLimits {
	result := RateLimits{ObservedAt: observedAt}
	subscription := RateLimitPool{ID: "subscription"}

	for _, id := range []string{rateLimitFiveHour, rateLimitSevenDay} {
		if window, ok := parseRateLimitWindow(id, payload[id], observedAt); ok {
			subscription.Windows = append(subscription.Windows, window)
		}
	}

	if len(subscription.Windows) > 0 {
		result.Pools = append(result.Pools, subscription)
	}

	for _, id := range []string{"seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet"} {
		if window, ok := parseRateLimitWindow(rateLimitSevenDay, payload[id], observedAt); ok {
			result.Pools = append(result.Pools, RateLimitPool{ID: id, Windows: []RateLimitWindow{window}})
		}
	}

	return appendModelRateLimits(result, payload["model_scoped"])
}

func appendModelRateLimits(result RateLimits, raw any) RateLimits {
	models, _ := raw.([]any)
	seen := make(map[string]bool, len(models))

	for _, rawModel := range models {
		model, _ := rawModel.(map[string]any)

		name, _ := model["display_name"].(string)
		if strings.TrimSpace(name) == "" || seen[name] {
			continue
		}

		seen[name] = true
		if window, ok := parseRateLimitWindow(rateLimitUsage, model, result.ObservedAt); ok {
			if name == rateLimitFableLabel {
				window.DurationSeconds = rateLimitFableSeconds
			}

			result.Pools = append(result.Pools, RateLimitPool{
				ID: "model:" + name, Label: name, Windows: []RateLimitWindow{window},
			})
		}
	}

	return result
}

func parseRateLimitWindow(id string, raw any, observedAt time.Time) (RateLimitWindow, bool) {
	payload, _ := raw.(map[string]any)

	percent, ok := payload["utilization"].(float64)
	if !ok || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 {
		return RateLimitWindow{}, false
	}

	window := RateLimitWindow{ID: id, UsedPercent: percent}

	if reset, ok := payload["resets_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, reset); err == nil {
			if !parsed.After(observedAt) {
				return RateLimitWindow{}, false
			}

			window.ResetsAt = parsed.UTC()
		}
	}

	return window, true
}
