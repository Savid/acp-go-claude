package claude

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// testProcessIsolation returns the isolated shape: a native identity that is
// never the identity running the test, so the fixture keeps describing a launch
// that has a privilege boundary to cross whoever runs it.
func testProcessIsolation() *ProcessIsolation {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == os.Geteuid() {
		uid++
	}
	if gid == os.Getegid() {
		gid++
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}
	if environment[envSearchPath] == "" {
		environment[envSearchPath] = "/usr/bin:/bin"
	}

	return &ProcessIsolation{
		UID: uint32(uid), GID: uint32(gid), BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}

func withTestProcessIsolation(options Options) Options {
	if options.ProcessIsolation == nil {
		options.ProcessIsolation = testProcessIsolation()
	}

	return options
}

// testTraversableTempDir returns a directory an unprivileged native identity
// can reach. The directory must live under the system temp root rather than the
// package working directory: a checkout sits under the invoking user's home,
// and Ubuntu creates home directories mode 0750, so no other identity can
// search into the checkout at all.
func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-claude-test-")
	if err != nil {
		t.Fatalf("create traversable test directory: %v", err)
	}
	if err = os.Chmod(directory, 0o711); err != nil {
		_ = os.RemoveAll(directory)
		t.Fatalf("make test directory traversable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}
