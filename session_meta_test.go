package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResumeCanDisableBareMode(t *testing.T) {
	t.Parallel()
	request := ResumeSessionRequest("stored-session", t.TempDir(), WithSessionClaudeOptions(NewClaudeOptions(WithClaudeBare(false))))
	meta, refusal := parseSessionMeta(request.Meta)
	require.Nil(t, refusal)
	options := inheritCarrier(meta, sessionRecord{Bare: true})
	require.False(t, options.Bare)
	inherited := inheritCarrier(sessionMeta{}, sessionRecord{Bare: true})
	require.True(t, inherited.Bare)
}
