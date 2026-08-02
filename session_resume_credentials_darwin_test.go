package claudeacp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadClaudeResumeKeychainCredentialAnswersAbsenceForAnUnwrittenHome(t *testing.T) {
	// A config dir nothing ever logged into owns no keychain item, so the
	// real keystore answers absence rather than an error. That is what keeps
	// file-authenticated and env-authenticated homes on the plaintext leg.
	data, err := readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "never-logged-in"))
	require.NoError(t, err)
	require.Nil(t, data)
}
