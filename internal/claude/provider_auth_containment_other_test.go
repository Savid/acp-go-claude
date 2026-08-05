//go:build !darwin

package claude

import "testing"

func useAuthDirectContainment(t *testing.T) {
	t.Helper()
	useDirectTestContainment(t)
}
