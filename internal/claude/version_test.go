package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
