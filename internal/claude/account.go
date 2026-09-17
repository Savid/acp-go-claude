package claude

import "context"

// AccountUsage is the get_usage control response, limited to the members the
// adapter reads. A home with no credential answers Available false and no
// plan, exactly like an account that has no allowance.
type AccountUsage struct {
	Plan      string        `json:"subscription_type"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	Available bool          `json:"rate_limits_available"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Windows   *UsageWindows `json:"rate_limits"`           //nolint:tagliatelle // Claude uses this native wire spelling.
}

// UsageWindows carries the generic per-window list; the fixed window members
// beside it repeat entries of the list and are not read.
type UsageWindows struct {
	Limits []UsageWindow `json:"limits"`
}

// UsageWindow is one allowance window.
type UsageWindow struct {
	Kind     string      `json:"kind"`
	Percent  float64     `json:"percent"`
	ResetsAt string      `json:"resets_at"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Scope    *UsageScope `json:"scope"`
}

// UsageScope narrows a window to one model when present.
type UsageScope struct {
	Model *UsageModel `json:"model"`
}

// UsageModel names the scoped model.
type UsageModel struct {
	DisplayName string `json:"display_name"` //nolint:tagliatelle // Claude uses this native wire spelling.
}

// AccountUsage reads the subscription allowance through the get_usage control
// request. skip_behaviors leaves out the usage-telemetry block the adapter
// never reads.
func (c *Client) AccountUsage(ctx context.Context) (AccountUsage, error) {
	var usage AccountUsage

	err := c.Control(ctx, map[string]any{keySubtype: "get_usage", "skip_behaviors": true}, &usage)

	return usage, err
}
