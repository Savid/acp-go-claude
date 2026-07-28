//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// containerGoArch maps the machine name the fixture reports to the Go
// architecture that builds for it. The value is read from the running container
// rather than from this host, because a test binary built for the wrong machine
// fails on exec and says nothing about a browser launcher.
var containerGoArch = map[string]string{
	"aarch64": "arm64",
	"x86_64":  "amd64",
}

// browserShimProof is the harness package's own non-execution test. It carries
// no build tag beyond `!windows`, so the same source that guards macOS compiles
// and runs unchanged on Linux.
const browserShimProof = "^TestLoginNeverExecsABrowserLauncher$"

// TestKeystoreLinuxLoginNeverExecsABrowserLauncher runs that proof inside the
// Linux fixture. `xdg-open`, `x-www-browser`, `www-browser` and
// `sensible-browser` are the names a Linux desktop answers, and a macOS run
// exercises none of them: it resolves `open` and leaves the rest of the shim
// asserted rather than executed. Only a Linux process on a Linux PATH decides
// whether the login child reaches a real launcher.
func TestKeystoreLinuxLoginNeverExecsABrowserLauncher(t *testing.T) {
	requireRunKeystore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := startKeystoreContainer(t, ctx)

	machine := strings.TrimSpace(execInKeystoreContainer(t, ctx, container, []string{"uname", "-m"}))

	goArch, ok := containerGoArch[machine]
	require.True(t, ok, "no Go architecture is mapped for machine %q", machine)

	binary := filepath.Join(t.TempDir(), "browser-shim.test")

	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", binary, "../internal/claude")
	build.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+goArch)

	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))

	const target = "/home/canary/browser-shim.test"

	require.NoError(t, container.CopyFileToContainer(ctx, binary, target, 0o755))

	// execInKeystoreContainer already fails on a non-zero exit code, and a Go
	// test binary that exits zero having selected nothing prints no PASS, so both
	// halves are needed before the absence of a launch means anything.
	proof := execInKeystoreContainer(t, ctx, container, []string{target, "-test.run", browserShimProof, "-test.v"})
	require.Contains(t, proof, "PASS")
}
