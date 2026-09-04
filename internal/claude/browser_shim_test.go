package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserShimEnvironSetsBothMechanismsAtOnce(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("shim", "dir")

	environ := browserShimEnviron([]string{
		"MALFORMED",
		"KEEP=value",
		envSearchPath + "=/usr/bin:/bin",
		browserShimBrowserEnv + "=/usr/bin/firefox",
	}, dir)

	require.Equal(t, []string{
		browserShimBrowserEnv + "=" + filepath.Join(dir, "open"),
		"KEEP=value",
		envSearchPath + "=" + dir + string(os.PathListSeparator) + "/usr/bin:/bin",
	}, environ)
}

func TestBrowserShimEnvironLeadsPathWhenTheChildInheritsNone(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("shim", "dir")

	require.Equal(t, []string{
		browserShimBrowserEnv + "=" + filepath.Join(dir, "open"),
		envSearchPath + "=" + dir,
	}, browserShimEnviron(nil, dir))
}

func TestBrowserShimRemoveToleratesNoShim(t *testing.T) {
	t.Parallel()

	var absent *browserShim

	require.NoError(t, absent.remove())
}
