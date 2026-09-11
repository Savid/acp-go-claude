package claude

import (
	"net/url"
	"slices"
	"strings"
)

const (
	directAPIOAuthTokenEnv      = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	directAPIKeyEnv             = "ANTHROPIC_API_KEY"       //nolint:gosec // Environment variable name, not a credential.
	directAPIBaseURLEnv         = "ANTHROPIC_BASE_URL"
	directAPIDefaultBase        = "https://api.anthropic.com"
	directAPIUnixSocketEnv      = "ANTHROPIC_UNIX_SOCKET"
	directAPISessionTokenEnv    = "CLAUDE_CODE_SESSION_ACCESS_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	directAPIWebSocketAuthFDEnv = "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR"
	directAPIAuthTokenEnv       = "ANTHROPIC_AUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential.
	directAPIFoundryEnv         = "CLAUDE_CODE_USE_FOUNDRY"
	directAPIGatewayLoginMethod = "gateway"
	directAPICustomHeadersEnv   = "ANTHROPIC_CUSTOM_HEADERS"
	directAPIVertexEnv          = "CLAUDE_CODE_USE_VERTEX"
	directAPIBedrockEnv         = "CLAUDE_CODE_USE_BEDROCK"
	directAPIInternalBaseURLEnv = "CLAUDE_CODE_API_BASE_URL"
	anthropicAPIHost            = "api.anthropic.com"

	// apiProviderFirstParty is the value Claude reports for a process that
	// talks the Anthropic API directly, whatever endpoint serves it. Every
	// other provider it names serves Anthropic's models under its own account.
	apiProviderFirstParty = "firstParty"
)

// assumeFirstPartyEnv tells Claude to treat whatever ANTHROPIC_BASE_URL names
// as its own endpoint, which is how an Anthropic-API pass-through in front of
// the real one is declared.
const assumeFirstPartyEnv = "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"

// firstPartyAPIHosts are the hostnames Claude answers for itself.
var firstPartyAPIHosts = []string{anthropicAPIHost, "api-staging.anthropic.com"}

// firstPartyRoute reports whether Claude resolved this process's requests to
// its own endpoint, by the test Claude applies to itself: the hostname alone
// decides, and the assume switch overrides it outright.
//
// An unset base URL, `https://api.anthropic.com/v1`, `http://api.anthropic.com`,
// `https://api-staging.anthropic.com`, and the assume switch set to `1`, `true`,
// `yes` or `on` each leave Claude serving its own menu and skipping gateway
// discovery; `0`, `false` and an empty value do not.
//
// It is deliberately not anthropicBaseURL: refusing to make a request over an
// odd spelling is safe, but refusing to publish a menu Claude is serving is not.
func firstPartyRoute(env map[string]string) bool {
	if claudeSwitchEnabled(env[EnvironmentKey(assumeFirstPartyEnv)]) {
		return true
	}

	base := strings.TrimSpace(env[EnvironmentKey(directAPIBaseURLEnv)])
	if base == "" {
		return true
	}

	endpoint, err := url.Parse(base)
	if err != nil {
		return false
	}

	return slices.Contains(firstPartyAPIHosts, strings.ToLower(endpoint.Hostname()))
}

// anthropicBaseURL reports whether ANTHROPIC_BASE_URL names Anthropic's own
// endpoint and nothing else. An unset value is Claude's own default, which is
// that endpoint; every other spelling — a path, a query, credentials, a
// non-default port, plain http — routes somewhere this adapter cannot vouch for.
func anthropicBaseURL(env map[string]string) bool {
	base := strings.TrimSpace(env[EnvironmentKey(directAPIBaseURLEnv)])
	if base == "" {
		return true
	}

	endpoint, err := url.Parse(base)

	return err == nil && endpoint.Scheme == authLoginURLScheme &&
		strings.EqualFold(endpoint.Hostname(), anthropicAPIHost) &&
		(endpoint.Port() == "" || endpoint.Port() == "443") && endpoint.User == nil &&
		(endpoint.Path == "" || endpoint.Path == "/") && endpoint.RawPath == "" &&
		endpoint.RawQuery == "" && !endpoint.ForceQuery && endpoint.Fragment == ""
}

// switchEnabledValues are the spellings Claude reads as on for a boolean
// switch; every other value, `0` and `false` included, is off.
const (
	switchValueTrue = "true"
	switchValueYes  = "yes"
)

var switchEnabledValues = []string{"1", switchValueTrue, switchValueYes, "on"}

// claudeSwitchEnabled applies Claude's truthy spellings for a boolean switch.
func claudeSwitchEnabled(value string) bool {
	return slices.Contains(switchEnabledValues, strings.ToLower(strings.TrimSpace(value)))
}

// These routes do not establish a captured credential for a direct API read.
// Each reader separately applies its API-key and endpoint eligibility rules.
func directAPIHasRouteOverride(env map[string]string) bool {
	for _, key := range []string{
		directAPIAuthTokenEnv, directAPICustomHeadersEnv, directAPIBedrockEnv,
		directAPIVertexEnv, directAPIFoundryEnv, "ANTHROPIC_AWS_API_KEY",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", directAPIUnixSocketEnv,
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
		"CLAUDE_BG_AUTH_SNAPSHOT_PATH", "CLAUDE_CODE_REMOTE", directAPISessionTokenEnv,
		directAPIWebSocketAuthFDEnv, "CLAUDE_CODE_WEBSOCKET_AUTH_TOKEN", "CLAUDE_SESSION_INGRESS_TOKEN_FILE",
	} {
		if strings.TrimSpace(env[EnvironmentKey(key)]) != "" {
			return true
		}
	}

	return false
}

// Effective settings are a disk/settings merge, not the process environment.
// Refuse differing credential or routing overrides rather than guessing which
// of them Claude applied; matching overrides preserve the captured identity.
func directAPISettingsMatch(settings map[string]any, env map[string]string) bool {
	if helper, exists := settings["apiKeyHelper"]; exists && helper != "" {
		return false
	}

	if method, exists := settings["forceLoginMethod"]; exists && method == directAPIGatewayLoginMethod {
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

		if directAPIIdentityEnv(key) && env[EnvironmentKey(key)] != value {
			return false
		}
	}

	return true
}

func directAPIIdentityEnv(key string) bool {
	key = strings.ToUpper(key)

	return strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_") ||
		strings.HasPrefix(key, "AWS_") || strings.HasPrefix(key, "AZURE_") || strings.HasPrefix(key, "GOOGLE_")
}
