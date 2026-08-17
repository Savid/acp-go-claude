//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	envRunKeystore    = "ACP_GO_CLAUDE_RUN_KEYSTORE"
	keystoreEnvFile   = "/run/acp-go-claude-keystore/env"
	keystoreRoundTrip = "/usr/local/bin/roundtrip.sh"
	keystoreProbePath = "/usr/local/bin/residence.test"
)

// requireRunKeystore gates the credential-residence tier on both env vars.
func requireRunKeystore(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run credential-residence tests", envRunIntegration, envRunKeystore)
	}
}

// requireKeystoreRuntime adds the container runtime to the tier's gates. Once
// the gates are set a missing runtime is a failure rather than a skip: a tier
// that skips itself away over its own preconditions reports nothing.
func requireKeystoreRuntime(t *testing.T) {
	t.Helper()

	requireRunKeystore(t)

	provider, err := testcontainers.ProviderDefault.GetProvider()
	require.NoError(t, err, "the keystore tier needs a container runtime")

	require.NoError(t, provider.Health(t.Context()), "the keystore tier needs a container runtime")
}

// TestKeystoreLinuxCredentialResidence drives the two Linux configurations of
// the residence matrix. Both run the same probe in the same container, differing
// only in whether the session bus that reaches the Secret Service is exported.
func TestKeystoreLinuxCredentialResidence(t *testing.T) {
	requireKeystoreRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := startKeystoreFixture(ctx, t)

	probe := buildResidenceProbe(t)
	require.NoError(t, container.CopyFileToContainer(ctx, probe, keystoreProbePath, 0o755))

	runResidenceMatrix(ctx, t, container, false)
	runResidenceMatrix(ctx, t, container, true)
}

// TestKeystoreLinuxArtifactCarriesNoSecretServiceClient pins the other half of
// the same fact from this repo's own side: the adapter compiled for Linux links
// no Secret Service client, so there is no code path for a live service to
// reach.
func TestKeystoreLinuxArtifactCarriesNoSecretServiceClient(t *testing.T) {
	requireRunKeystore(t)

	binary := filepath.Join(t.TempDir(), "acp-go-claude-linux")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../cmd/acp-go-claude")
	build.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH=amd64")

	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))

	contents, err := os.ReadFile(binary)
	require.NoError(t, err)

	for _, symbol := range []string{"libsecret", "org.freedesktop.secrets", "gnome-keyring"} {
		require.NotContains(t, string(contents), symbol)
	}
}

// startKeystoreFixture builds and starts the Secret Service fixture. Readiness
// is the store/lookup round trip the image ships and nothing else: a log line or
// a bus-name check both report ready against a service that answers no lookup,
// and the suite then goes green having tested the wrong thing.
func startKeystoreFixture(ctx context.Context, t *testing.T) testcontainers.Container {
	t.Helper()

	request := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    filepath.Join(".", "keystore"),
			Dockerfile: "Dockerfile",
			KeepImage:  true,
		},
		WaitingFor: wait.ForExec([]string{keystoreRoundTrip}).WithStartupTimeout(3 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: request,
		Started:          true,
	})
	require.NoError(t, err, "the keystore tier needs a container runtime; it fails rather than skips once its gate is set")

	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	return container
}

// buildResidenceProbe compiles the package that owns the credential read path
// into a test binary for the fixture's platform. GOWORK=off is not optional: a
// go.work in scope otherwise builds the probe from another module's
// requirements.
func buildResidenceProbe(t *testing.T) string {
	t.Helper()

	probe := filepath.Join(t.TempDir(), "residence.test")

	command := exec.CommandContext(t.Context(), "go", "test", "-c", "-tags=integration", "-o", probe, ".")
	command.Dir = ".."
	command.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))

	return probe
}

// runResidenceMatrix executes the probe in one configuration. It fails unless
// the logs carry the matrix's own PASS line: an exit status alone goes green on
// a skip, which is the silent success this tier exists to prevent.
func runResidenceMatrix(ctx context.Context, t *testing.T, container testcontainers.Container, bus bool) {
	t.Helper()

	name, prelude := "keystore-absent", ""
	if bus {
		name, prelude = "keystore-present", ". "+keystoreEnvFile+"; export DBUS_SESSION_BUS_ADDRESS; "
	}

	script := fmt.Sprintf("%sexport %s=1 %s=1; exec %s -test.v -test.run '^TestKeystoreResidenceMatrix$'",
		prelude, envRunIntegration, envRunKeystore, keystoreProbePath)

	t.Run(name, func(t *testing.T) {
		code, reader, err := container.Exec(ctx, []string{"sh", "-c", script}, tcexec.Multiplexed())
		require.NoError(t, err)

		logs := readExecOutput(t, reader)

		require.Zero(t, code, logs)
		require.Contains(t, logs, "--- PASS: TestKeystoreResidenceMatrix",
			"the matrix reported no pass")
	})
}

// readExecOutput drains a container exec stream. The stream ends in a read error
// often enough that treating one as a failure would fail the tier over output it
// already holds.
func readExecOutput(t *testing.T, reader io.Reader) string {
	t.Helper()

	output, err := io.ReadAll(reader)
	if err != nil {
		t.Logf("the exec stream ended in %v after %d bytes", err, len(output))
	}

	return string(output)
}
