package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectAPIReadersPreserveDistinctEligibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		env    map[string]string
		models bool
		quota  bool
	}{
		{name: "OAuth credential", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test"}, models: true, quota: true},
		{name: "API key", env: map[string]string{"ANTHROPIC_API_KEY": "key"}, models: true},
		{name: "both credentials", env: map[string]string{"ANTHROPIC_API_KEY": "key", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test"}},
		{name: "native API base override", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "CLAUDE_CODE_API_BASE_URL": "https://native.example"}, quota: true},
		{name: "custom Anthropic endpoint", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "ANTHROPIC_BASE_URL": "https://native.example"}, quota: true},
		{name: "configured false cloud flag", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "CLAUDE_CODE_USE_FOUNDRY": "0"}},
		{name: "blank cloud flag", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "CLAUDE_CODE_USE_FOUNDRY": " \t"}, models: true, quota: true},
		{name: "inherited remote credential", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "CLAUDE_CODE_SESSION_ACCESS_TOKEN": "remote"}},
		{name: "socket route", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "ANTHROPIC_UNIX_SOCKET": "/socket"}},
		{name: "descriptor credential", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-test", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR": "4"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := make(map[string]string, len(test.env))
			for key, value := range test.env {
				env[EnvironmentKey(key)] = value
			}
			_, models := resolveModelCatalogAccess(env)
			_, quota := resolveRateLimitsAPIAccess(env)
			require.Equal(t, test.models, models)
			require.Equal(t, test.quota, quota)
		})
	}
}
