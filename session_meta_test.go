package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

func TestResumeCanDisableBareMode(t *testing.T) {
	t.Parallel()
	request := wire.ResumeSessionRequest("stored-session", t.TempDir(), WithSessionClaudeOptions(NewClaudeOptions(WithClaudeBare(false))))
	meta, refusal := parseSessionMeta(request.Meta)
	require.Nil(t, refusal)
	options := inheritCarrier(meta, sessionRecord{Bare: true})
	require.False(t, options.Bare)
	inherited := inheritCarrier(sessionMeta{}, sessionRecord{Bare: true})
	require.True(t, inherited.Bare)
}

func TestSessionOptionCarrierCopiesMutableInputs(t *testing.T) {
	t.Parallel()
	env := map[string]string{"PATH": "/bin", "EMPTY": ""}
	dirs := []string{"/first", "/second"}
	options := NewClaudeOptions(WithClaudeEnv(env), WithClaudeExtraPathDirs(dirs...), WithClaudePermissionMode(permissionModePlan), WithClaudeBare(false))
	meta := options.Meta()
	parsed, refusal := parseSessionMeta(meta)
	require.Nil(t, refusal)
	require.True(t, parsed.presentEnv)
	require.True(t, parsed.presentExtraPathDirs)
	require.Equal(t, options, parsed.options)
	env["PATH"] = "mutated"
	dirs[0] = "/mutated"
	options.Env["EMPTY"] = "mutated"
	options.ExtraPathDirs[1] = "/mutated"
	require.Equal(t, map[string]string{"PATH": "/bin", "EMPTY": ""}, parsed.options.Env)
	require.Equal(t, []string{"/first", "/second"}, parsed.options.ExtraPathDirs)
}
