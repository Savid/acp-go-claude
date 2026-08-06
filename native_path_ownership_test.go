package claudeacp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// testIsolationIdentity is the identity a fixture isolates to. The effective
// identity cannot serve as-is: the policy forbids UID or GID zero, so a root
// test runner is rejected before reaching anything under test. Only a zero
// component is replaced, so an unprivileged runner keeps isolating to itself.
func testIsolationIdentity() (uint32, uint32) {
	uid, gid := os.Geteuid(), os.Getegid()
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
	}

	return uint32(uid), uint32(gid)
}

// testNativeOwnedHome materializes a Claude home the native-owned predicate
// admits: mode 0700, owned by the isolated identity, directly under the temp
// root so that identity can traverse its ancestry. t.TempDir cannot stand in —
// it nests its leaf under a 0700 directory no foreign identity may enter, and
// creates that leaf 0777&^umask, which umask 022 leaves group- and
// other-readable.
func testNativeOwnedHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "acp-go-claude-native-home-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	require.NoError(t, os.Chmod(home, 0o700))

	uid, gid := testIsolationIdentity()
	if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		require.NoError(t, os.Chown(home, int(uid), int(gid)))
	}

	return home
}
