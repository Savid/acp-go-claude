//go:build linux

package claude

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var errStandaloneClaimUnreachable = errors.New("standalone claim is unreachable under a shared identity")

// sharedSupervisorIsolation returns the shape an embedding hands a supervisor
// that never held a privilege to drop: its own identity, no borrowed
// capabilities, and no standalone owner fields.
func sharedSupervisorIsolation(uid uint32, gid uint32) *ProcessIsolation {
	return &ProcessIsolation{
		UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
	}
}

// sharedSupervisorSeams points every identity source at one unprivileged
// identity, so both arms are reachable whichever identity the suite runs as.
func sharedSupervisorSeams(t *testing.T, uid int) {
	t.Helper()
	restoreTurnSupervisorSeams(t)
	restoreSharedProcessIdentitySeams(t)

	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return uid }
	processEffectiveGID = func() int { return uid }
	turnSupervisorEffectiveUID = func() int { return uid }
}

// refuseSharedIdentityStandaloneClaim fails the test if the durable claim is
// reached at all: the arm exists because that claim cannot succeed for an
// identity a live task already holds.
func refuseSharedIdentityStandaloneClaim(t *testing.T) {
	t.Helper()

	turnSupervisorAcquireStandalone = func(
		uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
	) (*agentStandaloneIdentity, error) {
		t.Error("a shared identity reached the standalone claim")

		return nil, errStandaloneClaimUnreachable
	}
}

// sharedSupervisorConfig returns the guardian config the parent seals for a
// launch under its own identity.
func sharedSupervisorConfig(uid uint32) turnSupervisorConfig {
	return turnSupervisorConfig{
		Path:            "/bin/true",
		Args:            []string{"true"},
		Env:             []string{"PATH=/usr/bin:/bin"},
		Isolation:       *sharedSupervisorIsolation(uid, uid),
		AuthorityOrigin: turnSupervisorShared,
	}
}

// TestSupervisorIdentityRuleAcceptsTheIdentityItAlreadyRuns proves the arm the
// root assertion has to make room for. The supervisor demands the trusted root
// identity because it descends from it to reach the native one; when the native
// identity is the one it already runs as there is no descent to make, and the
// demand would refuse the only launch such a deployment can perform. Every
// refusal outside that shape stands exactly as before, including a non-root
// supervisor asked for an identity it does not hold.
func TestSupervisorIdentityRuleAcceptsTheIdentityItAlreadyRuns(t *testing.T) {
	sharedSupervisorSeams(t, 1000)

	require.NoError(t, validateTurnSupervisorIdentity(sharedSupervisorIsolation(1000, 1000)))
	require.NoError(t, validateTurnSupervisorIdentity(sharedSupervisorIsolation(1000, 1001)))

	require.ErrorContains(
		t,
		validateTurnSupervisorIdentity(sharedSupervisorIsolation(64251, 64252)),
		"trusted root identity is required, effective uid is 1000",
	)

	processEffectiveUID = func() int { return 0 }
	turnSupervisorEffectiveUID = func() int { return 0 }
	require.ErrorContains(
		t,
		validateTurnSupervisorIdentity(&ProcessIsolation{UID: 0, GID: 0}),
		"native target identity must differ from the trusted supervisor",
	)
	require.NoError(t, validateTurnSupervisorIdentity(sharedSupervisorIsolation(64251, 64252)))
}

// TestPrepareTurnSupervisorStampsTheAuthorityItsIdentityAllows proves the
// parent records which authority the tree will run under in the sealed config,
// so the guardian and the liveness supervisor inherit one decision instead of
// each inventing their own.
func TestPrepareTurnSupervisorStampsTheAuthorityItsIdentityAllows(t *testing.T) {
	sharedSupervisorSeams(t, 1000)

	var sealed turnSupervisorConfig

	turnSupervisorWriteConfig = func(file io.WriteSeeker, config turnSupervisorConfig) error {
		sealed = config

		return writeTurnSupervisorConfig(file, config)
	}

	launch, err := prepareProcessTreeCommand(
		exec.Command("/bin/true"),
		processLaunchOptions{Isolation: sharedSupervisorIsolation(1000, 1000)},
	)
	require.NoError(t, err)
	launch.close()

	require.Equal(t, turnSupervisorShared, sealed.AuthorityOrigin)
	require.False(t, sealed.IdentityLock)
	require.False(t, sealed.AuthorityDomain)

	processEffectiveUID = func() int { return 0 }
	turnSupervisorEffectiveUID = func() int { return 0 }

	launch, err = prepareProcessTreeCommand(
		exec.Command("/bin/true"),
		processLaunchOptions{Isolation: testProcessIsolation()},
	)
	require.NoError(t, err)
	launch.close()

	require.Equal(t, turnSupervisorStandalone, sealed.AuthorityOrigin)
}

// TestSupervisorConfigRefusesAnAuthorityOriginItsIdentityContradicts proves the
// stamp directs the launch without being trusted on its own. Each child derives
// the same decision from its own identity and refuses a config that disagrees,
// in both directions, so a stamp that survived into the wrong process cannot
// select steps that process must not take.
func TestSupervisorConfigRefusesAnAuthorityOriginItsIdentityContradicts(t *testing.T) {
	sharedSupervisorSeams(t, 1000)

	require.NoError(t, validateTurnSupervisorConfig(sharedSupervisorConfig(1000)))

	require.ErrorContains(
		t,
		validateTurnSupervisorConfig(sharedSupervisorConfig(64251)),
		"claude native supervisor authority origin does not match the identity it runs as",
	)

	standalone := supervisorCovConfig()
	standalone.Isolation.UID = 1000
	standalone.Isolation.GID = 1000
	require.ErrorContains(
		t,
		validateTurnSupervisorConfig(standalone),
		"claude native supervisor authority origin does not match the identity it runs as",
	)

	for _, testCase := range []struct {
		name   string
		mutate func(*turnSupervisorConfig)
	}{
		{
			name: "carrying capabilities",
			mutate: func(config *turnSupervisorConfig) {
				config.IdentityLock = true
				config.AuthorityDomain = true
			},
		},
		{
			name:   "carrying an owner id",
			mutate: func(config *turnSupervisorConfig) { config.Isolation.StandaloneOwnerID = "claude-shared" },
		},
		{
			name:   "carrying a state root",
			mutate: func(config *turnSupervisorConfig) { config.Isolation.StandaloneStateRoot = "/srv/claude" },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := sharedSupervisorConfig(1000)
			testCase.mutate(&config)
			require.ErrorContains(
				t,
				validateTurnSupervisorConfig(config),
				"claude shared supervisor authority disposition is invalid",
			)
		})
	}
}

// TestSharedIdentityGuardianTakesNoAgentAuthority proves the guardian claims
// nothing it cannot hold. The durable registry records who may enter an
// identity nobody is in; the supervisor is already in this one, so there is no
// claim to take, nothing to publish on the way out, and the standalone acquirer
// is never reached.
func TestSharedIdentityGuardianTakesNoAgentAuthority(t *testing.T) {
	sharedSupervisorSeams(t, 1000)

	refuseSharedIdentityStandaloneClaim(t)

	authority, err := acquireTurnSupervisorAuthority(sharedSupervisorConfig(1000), 7, 8, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, authority)
	require.Nil(t, authority.identity)
	require.Nil(t, authority.domain)
	require.Nil(t, authority.standalone)

	completion := &strings.Builder{}
	require.NoError(t, completeTurnSupervisorAuthority(completion, &authority, true))
	require.Equal(t, turnSupervisorProof, completion.String())
	require.Nil(t, authority)
}

// TestRunTurnSupervisorNativeLaunchesUnderASharedIdentity proves the liveness
// supervisor runs the whole unprivileged handshake with no authority at all: it
// arms, waits for the guardian's start gate, launches the native root without
// asking the kernel for a credential change, reports the pid, and contains the
// tree when the root exits.
func TestRunTurnSupervisorNativeLaunchesUnderASharedIdentity(t *testing.T) {
	sharedSupervisorSeams(t, 1000)

	turnSupervisorCoreLimit = func() error { return nil }
	turnSupervisorNoNewPrivs = func() error { return nil }
	turnSupervisorEnable = func() error { return nil }
	refuseSharedIdentityStandaloneClaim(t)

	contained := make(chan [2]int, 4)
	turnSupervisorContain = func(supervisor, native int) error {
		contained <- [2]int{supervisor, native}

		return nil
	}

	notified := make(chan chan<- os.Signal, 1)
	turnSupervisorSignalNotify = func(target chan<- os.Signal, _ ...os.Signal) { notified <- target }
	turnSupervisorSignalStop = func(chan<- os.Signal) {}

	config := sharedSupervisorConfig(1000)

	control, _ := supervisorCovPipe(t)
	ready := supervisorCovNewPublisher()
	completion := supervisorCovNewPublisher()

	require.NoError(t, runTurnSupervisorNative(
		supervisorCovEncode(t, config), []io.Reader{control}, nil, strings.NewReader("\x01"),
		ready, completion, 6, 7,
	))

	lines := ready.lines()
	require.Equal(t, turnSupervisorArmed, lines[0])
	require.True(t, strings.HasPrefix(lines[1], "ready:"), "liveness never reported a native pid: %q", lines)
	require.Equal(t, "done\n", lines[len(lines)-1])
	require.NotZero(t, (<-contained)[1])
}

// TestSharedIdentitySupervisorContainsARealNativeLaunch proves the whole tree
// runs for an identity that never held privilege: the guardian and the liveness
// supervisor self-exec, the readiness handshake completes, the native root runs
// as the supervisor's own identity, and its output is captured and contained.
// It is the launch the trusted-root suite performs, with the one difference
// this arm exists for.
func TestSharedIdentitySupervisorContainsARealNativeLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a shared agent identity requires an unprivileged supervisor")
	}

	isolation := sharedSupervisorIsolation(uint32(os.Geteuid()), uint32(os.Getegid()))

	output, err := containedClaudeOutput(
		t.Context(),
		"/bin/sh",
		[]string{"-c", `printf '2.1.80 (Claude Code)\n'`},
		Options{Cwd: "/", ProcessIsolation: isolation},
		nil,
		"claude version",
	)
	require.NoError(t, err)
	require.Equal(t, "2.1.80 (Claude Code)\n", string(output))
}
