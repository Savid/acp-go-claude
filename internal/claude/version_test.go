package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckMinimumVersion(t *testing.T) {
	t.Parallel()

	require.NoError(t, CheckMinimumVersion("2.1.270", MinimumVersion))
	require.NoError(t, CheckMinimumVersion("v2.0.0-beta", "2.0.0"))
	require.NoError(t, CheckMinimumVersion("1.0", "0.99.99"))
	require.Error(t, CheckMinimumVersion("1.9.9", "2.0.0"))
	require.Error(t, CheckMinimumVersion("abc", "2.0.0"))
	require.Error(t, CheckMinimumVersion("2.0.0", "x"))
	require.Error(t, CheckMinimumVersion("", "2.0.0"))
}

func TestProbeVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho 1.2.3\n"), 0o700))

	version, err := ProbeVersion(context.Background(), script, []string{"PATH=/usr/bin:/bin"})
	require.NoError(t, err)
	require.Equal(t, "1.2.3", version)

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("#!/bin/sh\n"), 0o700))
	_, err = ProbeVersion(context.Background(), empty, nil)
	require.ErrorContains(t, err, "empty claude version")

	_, err = ProbeVersion(context.Background(), filepath.Join(dir, "missing"), nil)
	require.Error(t, err)
}
