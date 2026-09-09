package claude

import "strings"

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
)

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
