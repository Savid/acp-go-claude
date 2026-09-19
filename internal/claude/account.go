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

// RateLimits is the native allowance report: the shared session and weekly
// windows as fixed members, the weekly windows scoped to one model, and
// monetary spending in the shared Anthropic shape.
type RateLimits struct {
	Session     *Window          `json:"five_hour"`    //nolint:tagliatelle // Claude uses this native wire spelling.
	Weekly      *Window          `json:"seven_day"`    //nolint:tagliatelle // Claude uses this native wire spelling.
	ModelScoped []ScopedWindow   `json:"model_scoped"` //nolint:tagliatelle // Claude uses this native wire spelling.
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
// vocabulary: the session window, the weekly window for all models, and one
// scoped weekly window per model. A window without a percentage is left out.
func (r RateLimits) Observation() anthropic.Observation {
	observation := anthropic.Observation{Spend: r.Spend}

	if r.Session != nil && r.Session.Utilization != nil {
		observation.Limits = append(observation.Limits, anthropic.Limit{Kind: QuotaSession, Percent: r.Session.Utilization, ResetsAt: r.Session.ResetsAt})
	}

	if r.Weekly != nil && r.Weekly.Utilization != nil {
		observation.Limits = append(observation.Limits, anthropic.Limit{Kind: QuotaWeekly, Percent: r.Weekly.Utilization, ResetsAt: r.Weekly.ResetsAt})
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
