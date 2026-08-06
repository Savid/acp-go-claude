package claudeacp

import (
	"os"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-claude/internal/claude"
)

// TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess proves the
// rule that decides whether the session may offer a mode that skips permission
// prompts entirely. An unprivileged agent may offer it. A root agent may offer
// it only when the environment explicitly declares a sandbox, because bypassing
// prompts as root means unreviewed tool calls run with full privilege.
func TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess(t *testing.T) {
	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")

	t.Cleanup(func() {
		osGeteuid = previousGeteuid

		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})

	require.NoError(t, os.Unsetenv("IS_SANDBOX"))

	osGeteuid = func() int { return 1000 }
	require.True(t, bypassPermissionsAvailable(), "an unprivileged agent may offer bypass mode")

	osGeteuid = func() int { return 0 }
	require.False(t, bypassPermissionsAvailable(), "a root agent offered bypass mode outside a sandbox")

	require.NoError(t, os.Setenv("IS_SANDBOX", "1"))
	require.True(t, bypassPermissionsAvailable(), "a declared sandbox did not re-enable bypass mode")
}

// TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable proves the advertised
// mode list is derived from that same rule rather than being fixed. A client
// that never sees the option cannot select it, so this is where the privilege
// rule actually reaches the protocol.
func TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable(t *testing.T) {
	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")

	t.Cleanup(func() {
		osGeteuid = previousGeteuid

		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})

	require.NoError(t, os.Unsetenv("IS_SANDBOX"))

	available := []claude.AvailableModelInfo{{Value: "opus", DisplayName: "Opus"}}
	bypass := acp.SessionConfigSelectOption{
		Name:  "Bypass Permissions",
		Value: acp.SessionConfigValueId(modeBypassPermissions),
	}

	osGeteuid = func() int { return 0 }
	require.NotContains(t, modeSelectOptions("opus", available), bypass)

	osGeteuid = func() int { return 1000 }

	offered := modeSelectOptions("opus", available)
	require.Contains(t, offered, bypass)
	require.Contains(t, offered, acp.SessionConfigSelectOption{
		Name: modeNameDefault, Value: acp.SessionConfigValueId(modeDefault),
	})
	require.Contains(t, offered, acp.SessionConfigSelectOption{
		Name: modeNameDontAsk, Value: acp.SessionConfigValueId(modeDontAsk),
	})
}

// TestProviderAuthSettingsWithoutCredentialsAreNotConfigured proves a settings
// file that parses but carries neither an apiKeyHelper nor any credential
// environment variable does not count as provider auth. Counting it would make
// the wrapper treat an unrelated Claude settings file as an existing
// credential and suppress its own brokering.
func TestProviderAuthSettingsWithoutCredentialsAreNotConfigured(t *testing.T) {
	require.False(t, providerAuthSettingsContentConfigured([]byte(`{}`)))
	require.False(t, providerAuthSettingsContentConfigured([]byte(`{"env":{"EDITOR":"vi"}}`)))
	require.False(
		t,
		providerAuthSettingsContentConfigured([]byte(`{"env":{"`+providerAuthEnvAnthropicAPIKey+`":"  "}}`)),
		"a blank credential value counted as configured",
	)

	require.True(
		t,
		providerAuthSettingsContentConfigured([]byte(`{"env":{"`+providerAuthEnvAnthropicAPIKey+`":"secret"}}`)),
	)
}
