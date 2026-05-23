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

	requireInvalidParams(t, validateRequiredAbsolutePath("cwd", ""))
	requireInvalidParams(t, validateRequiredAbsolutePath("cwd", "relative"))
	requireInvalidParams(t, validateOptionalAbsolutePath("cwd", acp.Ptr("relative")))
	requireInvalidParams(t, validateAbsolutePaths("additionalDirectories", []string{""}))
	requireInvalidParams(t, validateAbsolutePaths("additionalDirectories", []string{"relative"}))
	requireInvalidParams(t, validateSessionStartPaths("relative", nil))
}

func requireInvalidParams(t *testing.T, err error) {
	t.Helper()

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32602, reqErr.Code)
}
