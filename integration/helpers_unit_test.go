//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNullClaudeRefreshTokens(t *testing.T) {
	t.Parallel()

	data, err := nullClaudeRefreshTokens([]byte(`{
		"claudeAiOauth": {
			"accessToken": "access",
			"refreshToken": "refresh"
		},
		"nested": [{"refreshToken": "nested-refresh"}]
	}`))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))

	oauth, ok := decoded["claudeAiOauth"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "access", oauth["accessToken"])
	require.Contains(t, oauth, "refreshToken")
	require.Nil(t, oauth["refreshToken"])

	nested, ok := decoded["nested"].([]any)
	require.True(t, ok)
	nestedObj, ok := nested[0].(map[string]any)
	require.True(t, ok)
	require.Contains(t, nestedObj, "refreshToken")
	require.Nil(t, nestedObj["refreshToken"])
}

func TestClaudeAccessToken(t *testing.T) {
	t.Parallel()

	token := claudeAccessToken(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "access",
			"refreshToken": "refresh",
		},
	})
	require.Equal(t, "access", token)

	token = claudeAccessToken(map[string]any{
		"nested": []any{map[string]any{
			"claudeAiOauth": map[string]any{"accessToken": "nested-access"},
		}},
	})
	require.Equal(t, "nested-access", token)
}

func TestCopyClaudeStateFileUsesSiblingForDefaultHome(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	source := filepath.Join(parent, ".claude")
	target := t.TempDir()
	require.NoError(t, os.Mkdir(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(parent, ".claude.json"), []byte(`{"ok":true,"refreshToken":"secret"}`), 0o600))

	require.NoError(t, copyClaudeStateFile(source, target))

	var copied map[string]any
	data, err := os.ReadFile(filepath.Join(target, ".claude.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &copied))
	require.Equal(t, true, copied["ok"])
	require.Contains(t, copied, "refreshToken")
	require.Nil(t, copied["refreshToken"])
}

func TestClaudeSettingsAuthAvailable(t *testing.T) {
	t.Parallel()

	settings := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"token"}}`), 0o600))

	require.True(t, claudeSettingsAuthAvailable(t, settings))
}

func TestIsolatedClaudeRuntimeUsesFreshHomeWithProcessAuth(t *testing.T) {
	t.Setenv(envAnthropicAuthToken, "token")
	t.Setenv(envClaudeHome, "")

	runtime := isolatedClaudeRuntime(t)
	require.NotEmpty(t, runtime.home)
	require.DirExists(t, runtime.home)
	require.NoFileExists(t, filepath.Join(runtime.home, "settings.json"))
	require.NoFileExists(t, filepath.Join(runtime.home, ".credentials.json"))
}

func TestIsolatedClaudeRuntimeCopiesExplicitSource(t *testing.T) {
	// Process auth outranks a copied credential, so the explicit-source path is
	// only observable once the ambient live-auth variables are cleared.
	t.Setenv(envAnthropicAuthToken, "")
	t.Setenv(envAnthropicAPIKey, "")
	t.Setenv(envClaudeCodeOAuthToken, "")

	source := t.TempDir()
	t.Setenv(envClaudeHome, source)
	require.NoError(t, os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"settings-token"}}`), 0o600))

	runtime := isolatedClaudeRuntime(t)
	require.NotEmpty(t, runtime.home)
	require.NotEqual(t, source, runtime.home)
	require.FileExists(t, filepath.Join(runtime.home, ".credentials.json"))
	require.FileExists(t, filepath.Join(runtime.home, "settings.json"))
	require.Equal(t, map[string]string{envAnthropicAuthToken: "access"}, runtime.env)
}
