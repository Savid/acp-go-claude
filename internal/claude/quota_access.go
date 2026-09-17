package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"strings"
)

const defaultQuotaHaikuModel = "claude-haiku-4-5"

// QuotaAccess is the addressed process's verified setup-token route.
type QuotaAccess struct {
	token    string
	endpoint string
	haiku    string
	fable    string
}

func (a QuotaAccess) Key() [32]byte {
	return sha256.Sum256([]byte(a.token + "\x00" + a.endpoint + "\x00" + a.haiku + "\x00" + a.fable))
}

//nolint:nilnil // A nil access means the process is ineligible for quota probes.
func (c *Client) QuotaAccess(ctx context.Context, entries []string, bare bool) (*QuotaAccess, error) {
	access := quotaAccess(entries, bare)
	if access == nil {
		return nil, nil
	}

	var settings struct {
		Effective json.RawMessage `json:"effective"`
	}
	if err := c.Control(ctx, map[string]any{keySubtype: "get_settings"}, &settings); err != nil {
		return nil, err
	}

	if !quotaSettingsMatch(settings.Effective, entries) {
		return nil, nil
	}

	return access, nil
}

func quotaAccess(entries []string, bare bool) *QuotaAccess {
	env := quotaEnvironment(entries)
	if bare || strings.TrimSpace(env["CLAUDE_CODE_OAUTH_TOKEN"]) == "" {
		return nil
	}

	for _, key := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_UNIX_SOCKET",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "ANTHROPIC_AWS_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_BG_AUTH_SNAPSHOT_PATH",
		"CLAUDE_CODE_REMOTE", "CLAUDE_CODE_SESSION_ACCESS_TOKEN", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR",
		"CLAUDE_CODE_WEBSOCKET_AUTH_TOKEN", "CLAUDE_SESSION_INGRESS_TOKEN_FILE",
	} {
		if env[key] != "" {
			return nil
		}
	}

	base := env["ANTHROPIC_BASE_URL"]
	if base == "" {
		base = "https://api.anthropic.com"
	}

	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host != "api.anthropic.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil
	}

	haiku := defaultQuotaHaikuModel

	for _, key := range []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
		if env[key] != "" {
			haiku = env[key]
		}
	}

	fable := env["ANTHROPIC_DEFAULT_FABLE_MODEL"]
	if fable == "" {
		fable = "claude-fable-5-1"
	}

	if !strings.HasPrefix(haiku, "claude-haiku-") || !strings.HasPrefix(fable, "claude-fable-") {
		return nil
	}

	return &QuotaAccess{token: env["CLAUDE_CODE_OAUTH_TOKEN"], endpoint: "https://api.anthropic.com/v1/messages", haiku: haiku, fable: fable}
}

func quotaEnvironment(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}

	return env
}

func quotaSettingsMatch(raw json.RawMessage, entries []string) bool {
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil || settings == nil {
		return false
	}

	var helper, login string
	if value, ok := settings["apiKeyHelper"]; ok && json.Unmarshal(value, &helper) != nil {
		return false
	}

	if value, ok := settings["forceLoginMethod"]; ok && json.Unmarshal(value, &login) != nil {
		return false
	}

	if helper != "" || login == "gateway" {
		return false
	}

	var overrides map[string]string
	if value, ok := settings["env"]; ok && json.Unmarshal(value, &overrides) != nil {
		return false
	}

	env := quotaEnvironment(entries)

	for key, value := range overrides {
		for _, prefix := range []string{"ANTHROPIC_", "CLAUDE_CODE_", "AWS_", "AZURE_", "GOOGLE_"} {
			if strings.HasPrefix(key, prefix) && env[key] != value {
				return false
			}
		}
	}

	return true
}
