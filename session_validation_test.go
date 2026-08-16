package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestSessionPathValidationHelpers(t *testing.T) {
	t.Parallel()

	abs := t.TempDir()
	require.NoError(t, validateRequiredAbsolutePath("cwd", abs))
	require.NoError(t, validateOptionalAbsolutePath("cwd", nil))
	require.NoError(t, validateOptionalAbsolutePath("cwd", acp.Ptr("")))
	require.NoError(t, validateOptionalAbsolutePath("cwd", &abs))
	require.NoError(t, validateAbsolutePaths("additionalDirectories", []string{abs}))
	require.NoError(t, validateSessionStartPaths(abs, []string{abs}))

	requireExactUnsupportedField(t, validateRequiredAbsolutePath("cwd", ""), "cwd")
	requireExactUnsupportedField(t, validateRequiredAbsolutePath("cwd", "relative"), "cwd")
	requireExactUnsupportedField(t, validateOptionalAbsolutePath("cwd", acp.Ptr("relative")), "cwd")

	// The index rides in the field path, so a rejected entry is never echoed
	// back to the caller that sent it.
	requireExactUnsupportedField(t, validateAbsolutePaths("additionalDirectories", []string{""}), "additionalDirectories[0]")
	requireExactUnsupportedField(
		t,
		validateAbsolutePaths("additionalDirectories", []string{abs, "relative"}),
		"additionalDirectories[1]",
	)
	requireExactUnsupportedField(t, validateSessionStartPaths("relative", nil), "cwd")
	requireExactUnsupportedField(t, validateSessionStartPaths(abs, []string{"relative"}), "additionalDirectories[0]")
}
