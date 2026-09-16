package claude

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestProjectDirNameSanitizes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ path, want string }{
		"absolute path":   {"/home/user/proj", "-home-user-proj"},
		"dots and dashes": {"/a.b/c-d", "-a-b-c-d"},
		"digits kept":     {"/p1/2q", "-p1-2q"},
		"empty path":      {"", "-"},
		"bmp non-ascii":   {"/caf\u00e9", "-caf-"},
		// An astral rune is two UTF-16 code units, so it sanitizes to two dashes.
		"astral non-ascii": {"/a\U0001F600b", "-a--b"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, ProjectDirName(tc.path))
		})
	}
}

func TestProjectDirNameTruncatesAndHashes(t *testing.T) {
	t.Parallel()

	exact := "/" + strings.Repeat("a", projectDirMaxUnits-1)
	require.Len(t, utf16.Encode([]rune(exact)), projectDirMaxUnits)
	require.Equal(t, "-"+strings.Repeat("a", projectDirMaxUnits-1), ProjectDirName(exact),
		"a name of exactly the limit is written verbatim")

	long := exact + "b"
	name := ProjectDirName(long)
	prefix, suffix, found := strings.Cut(name, "-"+strings.Repeat("a", projectDirMaxUnits-1)+"-")
	require.True(t, found, "a longer name keeps the truncated prefix and appends a hash")
	require.Empty(t, prefix)
	require.NotEmpty(t, suffix)
	require.Equal(t, name, ProjectDirName(long), "the name is stable for one path")

	// Two paths that share the truncated prefix must not share a directory.
	require.NotEqual(t, name, ProjectDirName(exact+"c"))
}
