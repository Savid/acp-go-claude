package claude

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func testProcessIsolation() *ProcessIsolation {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
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

func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve test working directory: %v", err)
	}
	directory, err := os.MkdirTemp(workingDirectory, "acp-go-claude-test-")
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
