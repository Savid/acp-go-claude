package claude

import (
	"context"

	"github.com/savid/acp-go-core/usage/anthropic"
)

// AccountUsage is the get_usage control response, limited to the members the
// adapter reads. A home with no credential answers Available false and no
// plan, exactly like an account that has no allowance.
type AccountUsage struct {
	Plan      string                 `json:"subscription_type"`     //nolint:tagliatelle // Claude uses this native wire spelling.
	Available bool                   `json:"rate_limits_available"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Windows   *anthropic.Observation `json:"rate_limits"`           //nolint:tagliatelle // Claude uses this native wire spelling.
}

// AccountUsage reads the subscription allowance through the get_usage control
// request. skip_behaviors leaves out the usage-telemetry block the adapter
// never reads.
func (c *Client) AccountUsage(ctx context.Context) (AccountUsage, error) {
	var usage AccountUsage

	err := c.Control(ctx, map[string]any{keySubtype: "get_usage", "skip_behaviors": true}, &usage)

	return usage, err
}
