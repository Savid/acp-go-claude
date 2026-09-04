package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionStorePathHelpers(t *testing.T) {
	t.Parallel()

	require.False(t, validUUIDShape("short"))
	require.False(t, validUUIDShape("11111111_1111-4111-8111-111111111111"))
	require.False(t, validUUIDShape("11111111-1111-4111-8111-11111111111g"))
	require.True(t, validUUIDShape("11111111-1111-4111-8111-111111111111"))
	require.True(t, validUUIDShape("AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"))

	for _, subpath := range []string{"", "/abs", `\abs`, "a\x00b", "a/./b", "a/../b", "a:/b"} {
		require.False(t, isSafeSessionSubpath(subpath), subpath)
	}
	require.True(t, isSafeSessionSubpath("sub/path.jsonl"))

	_, err := projectKeyForDirectory("")
	require.ErrorContains(t, err, "cwd is required")
	_, err = projectKeyForDirectory("relative")
	require.ErrorContains(t, err, "absolute")
	key, err := projectKeyForDirectory(t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, key)

	// A resume materializes the transcript under this key. Claude Code truncates
	// a project directory name longer than 200 characters and appends a hash, so
	// an untruncated key would hide the transcript from `claude --resume`. The
	// boundary sits on the sanitized name, whose spelling starts at the platform's
	// root, so each platform states its own pair.
	requireProjectKeyTruncationBoundary(t)
}
