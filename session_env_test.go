package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func simulatePlatform(t *testing.T, platform string) {
	t.Helper()

	previous := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = previous })

	runtimeGOOS = platform
}

func envMeta(env map[string]any) map[string]any {
	return map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: env}}}
}

func TestSessionEnvAcceptsEveryStructurallyValidName(t *testing.T) {
	simulatePlatform(t, "linux")

	env := map[string]any{
		"https_proxy":     "",
		"no_proxy":        "",
		"WAGIE_API_URL":   "http://127.0.0.1:1",
		"BASH_FUNC_x%%":   "() { :; }",
		"path":            "/not/the/search/path",
		"env":             "/not/the/shell/init",
		"ld_preload":      "/not/the/loader",
		"Xdg_Config_Home": "/not/the/managed/root",
	}

	options, err := claudeOptionsFromMeta(envMeta(env))
	require.NoError(t, err)
	require.Len(t, options.Env, len(env))
	require.Equal(t, "", options.Env["https_proxy"])
	require.NoError(t, ValidateClaudeSessionMeta(envMeta(env)))
}

func TestSessionEnvRefusesBlockedNamesUnderThePlatformIdentity(t *testing.T) {
	blocked := []string{
		"PATH", "NODE_OPTIONS", "BASH_ENV", "ENV", "CLAUDECODE",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		"HOME", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR",
	}

	for _, platform := range []string{"linux", "windows"} {
		simulatePlatform(t, platform)

		for _, key := range blocked {
			_, err := claudeOptionsFromMeta(envMeta(map[string]any{key: "x"}))
			requireExactUnsupportedField(t, err, metaOptionPath(settingsFieldEnv)+"."+key)
		}

		for _, key := range []string{privateAdapterEnvPrefix + "TOKEN", "acp_go_claude_internal_token"} {
			_, err := claudeOptionsFromMeta(envMeta(map[string]any{key: "x"}))
			requireExactUnsupportedField(t, err, metaOptionPath(settingsFieldEnv)+"."+key)
		}
	}

	simulatePlatform(t, "windows")

	for _, key := range []string{"path", "Node_Options", "ld_preload", "home", "xdg_state_home", "claudecode"} {
		_, err := claudeOptionsFromMeta(envMeta(map[string]any{key: "x"}))
		requireExactUnsupportedField(t, err, metaOptionPath(settingsFieldEnv)+"."+key)
	}
}

func TestSessionEnvReportsTheFirstKeyInSortedOrder(t *testing.T) {
	simulatePlatform(t, "linux")

	_, err := claudeOptionsFromMeta(envMeta(map[string]any{
		"ZZ_LAST":   "x\x00y",
		"AA_FIRST=": "x",
		"MM_MID":    "x",
	}))
	requireExactUnsupportedField(t, err, metaOptionPath(settingsFieldEnv)+".AA_FIRST=")
}

func TestSessionEnvRefusesTwoSpellingsOfOneWindowsVariable(t *testing.T) {
	env := map[string]any{"Https_Proxy": "a", "https_proxy": "b"}

	simulatePlatform(t, "linux")

	options, err := claudeOptionsFromMeta(envMeta(env))
	require.NoError(t, err)
	require.Len(t, options.Env, 2)

	simulatePlatform(t, "windows")

	_, err = claudeOptionsFromMeta(envMeta(env))
	require.Equal(t, ambiguousField(metaOptionPath(settingsFieldEnv)+".https_proxy"), err)
	require.Equal(t, ambiguousField(metaOptionPath(settingsFieldEnv)+".https_proxy"), ValidateClaudeSessionMeta(envMeta(env)))
}

func TestValidateClaudeSessionMetaMirrorsTheSessionParser(t *testing.T) {
	require.NoError(t, ValidateClaudeSessionMeta(nil))
	require.NoError(t, ValidateClaudeSessionMeta(NewClaudeOptions(
		WithClaudeEnv(map[string]string{"https_proxy": "", "WAGIE_API_TOKEN": "bearer"}),
		WithClaudeExtraPathDirs(absTestPath("session", "bin")),
	).Meta()))
	requireExactUnsupportedField(t, ValidateClaudeSessionMeta(envMeta(map[string]any{"PATH": "/bin"})), metaOptionPath(settingsFieldEnv)+".PATH")
	requireExactUnsupportedField(t, ValidateClaudeSessionMeta(map[string]any{claudeMetaKey: map[string]any{"extra": true}}), "_meta.claude.extra")
}
