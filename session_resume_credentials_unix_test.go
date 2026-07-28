//go:build integration && (linux || darwin)

package claudeacp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

const (
	envRunIntegration = "ACP_GO_CLAUDE_RUN_INTEGRATION"
	envRunKeystore    = "ACP_GO_CLAUDE_RUN_KEYSTORE"

	// envSessionBus reaches the Secret Service. Whether the fixture exported one
	// is the whole difference between the two Linux configurations.
	envSessionBus = "DBUS_SESSION_BUS_ADDRESS"

	// keystoreFixtureMarker is written by the credential-residence fixture's
	// entrypoint. Seeding a live Secret Service is only safe inside that
	// container, so the Linux configurations run nowhere else.
	keystoreFixtureMarker = "/run/acp-go-claude-keystore/marker"

	// keystoreDarwinService is a service name this test owns end to end, so the
	// macOS third never reads, overwrites, or deletes a real login item.
	keystoreDarwinService = "acp-go-claude-residence-canary"

	// keystoreDarwinConfigDir is the config dir the macOS removal half derives
	// production-shaped item names from. The names are a sha256 of this string,
	// so a fixed synthetic value keeps them deterministic and owned by this test
	// while putting them out of reach of the names any real config dir owns.
	keystoreDarwinConfigDir = "/acp-go-claude/residence-matrix/synthetic-config-dir"
)

// residenceCanary is the only material this matrix ever plants. It is not a
// credential and never was one.
const residenceCanary = `{"claudeAiOauth":{"accessToken":"canary-not-a-real-token"}}`

// residenceCanaryToken is the substring an accidental keystore read would carry
// into an answer that is supposed to come from the file store alone.
const residenceCanaryToken = "canary-not-a-real-token"

// residenceLinuxConfigDir and residenceLinuxAccount are the config dir and user
// the fixture container runs as. AuthKeychainItems derives every documented
// claude service name from that pair.
const (
	residenceLinuxConfigDir = "/root/.claude"
	residenceLinuxAccount   = "root"
)

// TestKeystoreResidenceMatrix pins where a claude credential lives, in all three
// configurations the tier drives: keystore-absent Linux, keystore-present Linux,
// and macOS. The plaintext store under the config dir is unconditionally
// authoritative — a live keystore holding a canary under every documented claude
// service name changes no answer the read path gives — and the file it reads is
// 0600.
func TestKeystoreResidenceMatrix(t *testing.T) {
	requireResidenceTier(t)
	seedResidenceKeystore(t)

	configDir := t.TempDir()

	// A seeded keystore is not a credential source. With no file under the config
	// dir the read path reports absence rather than reaching for the canary it
	// could otherwise find.
	absent, err := readClaudeResumeCredential(configDir)
	require.NoError(t, err)
	require.Nil(t, absent, "the read path answered from something other than the file store")

	credentialPath := filepath.Join(configDir, claudeResumeCredentialFile)
	require.NoError(t, os.WriteFile(credentialPath, []byte(residenceCanary), 0o600))

	present, err := readClaudeResumeCredential(configDir)
	require.NoError(t, err)
	require.Equal(t, residenceCanary, string(present))

	info, err := os.Stat(credentialPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	assertResidenceKeystoreCanary(t)
}

// requireResidenceTier answers to both tier gates. On Linux it additionally
// requires the fixture container: planting a canary in a developer's live
// Secret Service is not something a test may do, and the container is where the
// driver runs this binary once per Linux configuration.
func requireResidenceTier(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run the credential-residence matrix",
			envRunIntegration, envRunKeystore)
	}

	if runtime.GOOS == "darwin" {
		return
	}

	if _, err := os.Stat(keystoreFixtureMarker); err != nil {
		t.Skipf("the Linux configurations run inside the keystore fixture container: %v", err)
	}
}

// seedResidenceKeystore plants canary material through the platform tool, never
// through the read path. The Linux seeds land in a container that mounts no real
// home and no real store; the macOS seed lands in the login keychain under a
// service name this test owns and deletes.
func seedResidenceKeystore(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "darwin" {
		seedDarwinResidenceKeystore(t)

		return
	}

	if os.Getenv(envSessionBus) == "" {
		t.Log("no session bus is exported: this is the keystore-absent Linux configuration")

		return
	}

	for _, item := range claude.AuthKeychainItems(residenceLinuxConfigDir, residenceLinuxAccount) {
		store := exec.CommandContext(t.Context(), "secret-tool", "store",
			"--label=residence", "service", item.Service, "account", item.Account)
		store.Stdin = strings.NewReader(residenceCanary)

		output, err := store.CombinedOutput()
		require.NoError(t, err, string(output))
	}
}

// seedDarwinResidenceKeystore writes the canary to the real login keychain. A
// keychain write under a scratch HOME blocks forever on an interactive modal, so
// no writing harness is ever pointed at one.
func seedDarwinResidenceKeystore(t *testing.T) {
	t.Helper()

	account := residenceDarwinAccount(t)

	add := exec.CommandContext(t.Context(), "security", "add-generic-password",
		"-U", "-s", keystoreDarwinService, "-a", account, "-w", residenceCanary)

	output, err := add.CombinedOutput()
	require.NoError(t, err, string(output))

	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password",
			"-s", keystoreDarwinService, "-a", account).Run()
	})
}

// assertResidenceKeystoreCanary proves the keystore configuration this run was
// launched in is the one it claims. Without it a keystore-present run that
// silently lost its service would assert the same thing twice.
func assertResidenceKeystoreCanary(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "darwin" {
		assertDarwinResidenceCanary(t)

		return
	}

	if os.Getenv(envSessionBus) == "" {
		lookup := exec.CommandContext(t.Context(), "secret-tool", "lookup",
			"service", "acp-go-claude-residence", "account", residenceLinuxAccount)
		output, _ := lookup.CombinedOutput()
		require.NotContains(t, string(output), residenceCanaryToken,
			"a Secret Service answered in the keystore-absent configuration")

		return
	}

	for _, item := range claude.AuthKeychainItems(residenceLinuxConfigDir, residenceLinuxAccount) {
		lookup := exec.CommandContext(t.Context(), "secret-tool", "lookup",
			"service", item.Service, "account", item.Account)

		output, err := lookup.Output()
		require.NoError(t, err, "the seeded item is not readable")
		require.Equal(t, residenceCanary, string(output))
	}
}

// assertDarwinResidenceCanary reads the seeded item back, then hands over to the
// removal half.
func assertDarwinResidenceCanary(t *testing.T) {
	t.Helper()

	account := residenceDarwinAccount(t)

	found := exec.CommandContext(t.Context(), "security", "find-generic-password",
		"-s", keystoreDarwinService, "-a", account, "-w")

	value, err := found.Output()
	require.NoError(t, err)
	require.Equal(t, residenceCanary, strings.TrimSpace(string(value)))

	assertDarwinRemovalClearsPresentItems(t, account)
}

// assertDarwinRemovalClearsPresentItems drives the removal ladder against items
// that are actually there: both items per config dir across both reachable name
// shapes, seeded under the synthetic config dir and read back before the removal
// so their presence is established. Run against a config dir nothing ever wrote,
// the ladder only ever exercises its absence answer, which holds whether or not
// it can delete anything.
func assertDarwinRemovalClearsPresentItems(t *testing.T, account string) {
	t.Helper()

	items := claude.AuthKeychainItems(keystoreDarwinConfigDir, account)
	require.NotEmpty(t, items)

	for _, item := range items {
		add := exec.CommandContext(t.Context(), "security", "add-generic-password",
			"-U", "-s", item.Service, "-a", item.Account, "-w", residenceCanary)

		output, err := add.CombinedOutput()
		require.NoError(t, err, string(output))

		t.Cleanup(func() {
			_ = exec.Command("security", "delete-generic-password",
				"-s", item.Service, "-a", item.Account).Run()
		})
	}

	for _, item := range items {
		find := exec.CommandContext(t.Context(), "security", "find-generic-password",
			"-s", item.Service, "-a", item.Account)
		require.NoError(t, find.Run(), "item %q was not seeded, so removing it proves nothing", item.Service)
	}

	require.NoError(t, claude.RemoveAuthKeychainItems(t.Context(), keystoreDarwinConfigDir, account))

	for _, item := range items {
		find := exec.CommandContext(t.Context(), "security", "find-generic-password",
			"-s", item.Service, "-a", item.Account)
		require.Error(t, find.Run(), "item %q survived the removal ladder", item.Service)
	}
}

// residenceDarwinAccount names the account half of the item this test owns.
func residenceDarwinAccount(t *testing.T) string {
	t.Helper()

	account := os.Getenv("USER")
	require.NotEmpty(t, account, "the macOS third names its own keychain item after $USER")

	return account
}
