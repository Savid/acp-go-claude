//go:build darwin

package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessSysProcAttrDarwin(t *testing.T) {
	attr := processSysProcAttr()

	require.True(t, attr.Setpgid)
}
