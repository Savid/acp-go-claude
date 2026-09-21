package claude

import (
	"context"

	"github.com/savid/acp-go-core/usage/anthropic"
)

// AccountUsage is the get_usage control response, limited to the members the
// adapter reads. A home with no credential answers Available false and no
// plan, exactly like an account that has no allowance.
type AccountUsage struct {
	Plan      string      `json:"subscription_type"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	Available bool        `json:"rate_limits_available"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Windows   *RateLimits `json:"rate_limits"`           //nolint:tagliatelle // Claude uses this native wire spelling.
}

// RateLimits is the native allowance report with fixed and model-scoped
// windows and explicitly denominated monetary spending.
type RateLimits struct {
	Session     *Window          `json:"five_hour"`            //nolint:tagliatelle // Claude uses this native wire spelling.
	Weekly      *Window          `json:"seven_day"`            //nolint:tagliatelle // Claude uses this native wire spelling.
	OAuthApps   *Window          `json:"seven_day_oauth_apps"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Opus        *Window          `json:"seven_day_opus"`       //nolint:tagliatelle // Claude uses this native wire spelling.
	Sonnet      *Window          `json:"seven_day_sonnet"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	ModelScoped []ScopedWindow   `json:"model_scoped"`         //nolint:tagliatelle // Claude uses this native wire spelling.
	Spend       *anthropic.Spend `json:"spend"`
}

// Window is one allowance window's used percentage and reset time.
type Window struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"` //nolint:tagliatelle // Claude uses this native wire spelling.
}

// ScopedWindow is a weekly window limited to the named model.
type ScopedWindow struct {
	Window

	DisplayName string `json:"display_name"` //nolint:tagliatelle // Claude uses this native wire spelling.
}

// Observation projects the report onto the shared Anthropic allowance
// vocabulary. Fixed native windows and model-scoped entries retain distinct
// identities. A window without a percentage is left out.
func (r RateLimits) Observation() anthropic.Observation {
	observation := anthropic.Observation{Spend: r.Spend}

	for _, fixed := range []struct {
		kind   string
		window *Window
	}{
		{QuotaSession, r.Session},
		{QuotaWeekly, r.Weekly},
		{"seven_day_oauth_apps", r.OAuthApps},
		{"seven_day_opus", r.Opus},
		{"seven_day_sonnet", r.Sonnet},
	} {
		if fixed.window != nil && fixed.window.Utilization != nil {
			observation.Limits = append(observation.Limits, anthropic.Limit{Kind: fixed.kind, Percent: fixed.window.Utilization, ResetsAt: fixed.window.ResetsAt})
		}
	}

	for _, window := range r.ModelScoped {
		if window.Utilization == nil {
			continue
		}

		observation.Limits = append(observation.Limits, anthropic.Limit{
			Kind: QuotaScoped, Percent: window.Utilization, ResetsAt: window.ResetsAt,
			Scope: &anthropic.Scope{Model: &anthropic.Model{DisplayName: window.DisplayName}},
		})
	}

	return observation
}

// AccountUsage reads the subscription allowance through the get_usage control
// request. skip_behaviors leaves out the usage-telemetry block the adapter
// never reads.
func (c *Client) AccountUsage(ctx context.Context) (AccountUsage, error) {
	var usage AccountUsage

	err := c.Control(ctx, map[string]any{keySubtype: "get_usage", "skip_behaviors": true}, &usage)

	return usage, err
}
