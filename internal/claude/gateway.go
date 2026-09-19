package claude

import (
	"net/url"

	"github.com/savid/acp-go-core/usage/gateway"
)

// GatewayRoutes is the route claude sends Anthropic requests through when the
// environment points ANTHROPIC_BASE_URL away from Anthropic: that base with
// the auth token or API key it carries. Anthropic's own endpoint is no gateway.
func GatewayRoutes(entries []string) []gateway.Route {
	env := quotaEnvironment(entries)

	base := env["ANTHROPIC_BASE_URL"]
	if base == "" {
		return nil
	}

	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "api.anthropic.com" {
		return nil
	}

	token := env["ANTHROPIC_AUTH_TOKEN"]
	if token == "" {
		token = env["ANTHROPIC_API_KEY"]
	}

	return []gateway.Route{{Provider: "ANTHROPIC_BASE_URL", BaseURL: base, Token: token}}
}
