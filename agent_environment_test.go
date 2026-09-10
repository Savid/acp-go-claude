package claudeacp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWithAmbientEnvironmentReplacesTheAdapterEnvironment(t *testing.T) {
	original := captureAmbientEnvironment
	t.Cleanup(func() { captureAmbientEnvironment = original })

	captureAmbientEnvironment = func() []string {
		t.Error("the adapter environment was read despite a supplied ambient block")

		return []string{"PATH=/adapter/bin"}
	}

	agent := NewAgent(WithAmbientEnvironment(map[string]string{
		"HOME":        "/host/home",
		"PATH":        "/host/bin",
		"GOTRACEBACK": "crash",
	}))
	require.NoError(t, agent.configurationErr)
	require.Equal(t, map[string]string{"HOME": "/host/home", "PATH": "/host/bin"}, agent.ordinaryEnv)

	require.Empty(t, NewAgent(WithAmbientEnvironment(map[string]string{})).ordinaryEnv)
}

func TestWithAmbientEnvironmentRefusesMalformedEntries(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"empty key":  {env: map[string]string{"": "x"}, want: `key "" is not a variable name`},
		"equals key": {env: map[string]string{"A=B": "x"}, want: `key "A=B" is not a variable name`},
		"nul key":    {env: map[string]string{"A\x00B": "x"}, want: "is not a variable name"},
		"nul value":  {env: map[string]string{"A": "x\x00y"}, want: `value for "A" contains NUL`},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, NewAgent(WithAmbientEnvironment(tc.env)).configurationErr, tc.want)
		})
	}
}

func TestClaudeConfigDirFollowsTheNativeBaseEnvironment(t *testing.T) {
	ambient := NewAgent(WithAmbientEnvironment(map[string]string{"HOME": "/host/home", "PATH": "/host/bin"}))
	require.Equal(t, filepath.Join("/host/home", ".claude"), ambient.claudeConfigDir(""))
	require.Equal(t, filepath.Clean("/explicit/claude"), ambient.claudeConfigDir(" /explicit/claude/ "),
		"WithHome names the directory outright")

	require.Empty(t, NewAgent(WithAmbientEnvironment(map[string]string{"PATH": "/host/bin"})).claudeConfigDir(""),
		"a base without a home names no config dir")

	authority := &callbackHostAuthority{environment: func() map[string]string {
		return map[string]string{"HOME": "/native/home", "PATH": "/native/bin"}
	}}
	managed := NewAgent(WithHostAuthority(authority), WithAmbientEnvironment(map[string]string{"HOME": "/host/home"}))
	require.Equal(t, filepath.Join("/native/home", ".claude"), managed.claudeConfigDir(""),
		"managed execution follows the authority's environment, never the ambient block")
}
