//go:build linux

package claude

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessSysProcAttrLinux(t *testing.T) {
	attr := processSysProcAttr()

	require.True(t, attr.Setpgid)
	require.Equal(t, syscall.SIGKILL, attr.Pdeathsig)
}
