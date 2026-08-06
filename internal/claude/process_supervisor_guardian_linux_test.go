//go:build linux

package claude

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// supervisorCovGuardian drives runTurnSupervisorGuardian in place of the
// re-exec the parent normally performs. It owns the test side of every channel
// the guardian inherits — completion proof, control, start gate — and stands a
// shell script in for the liveness helper so each step of the handshake can be
// deviated from on purpose.
type supervisorCovGuardian struct {
	config       io.Reader
	control      *os.File
	controlWrite *os.File
	startGate    *os.File
	completion   *os.File
	ready        *supervisorCovPublisher
	notified     chan chan<- os.Signal
	contained    chan [2]int
	opened       *[]uintptr
}

// supervisorCovLiveness is the liveness stub's descriptor layout. The guardian
// hands its helper eight extra files, so the data channel it reports on is fd 5
// and the start gate it waits behind is fd 10.
const (
	supervisorCovLivenessData      = "5"
	supervisorCovLivenessControl   = "4"
	supervisorCovLivenessStartGate = "10"
)

// supervisorCovGuardianFixture installs every seam the guardian body needs and
// returns the test's side of its inherited channels. script runs in place of the
// liveness helper; an empty script means the guardian is expected to fail before
// it launches one.
func supervisorCovGuardianFixture(
	t *testing.T, config turnSupervisorConfig, script string,
) *supervisorCovGuardian {
	t.Helper()

	return supervisorCovGuardianFixtureAt(t, config, script, 7)
}

// supervisorCovGuardianFixtureAt is supervisorCovGuardianFixture with an
// explicit start-gate descriptor number, so a scenario can withhold the gate the
// guardian is supposed to ask for.
func supervisorCovGuardianFixtureAt(
	t *testing.T, config turnSupervisorConfig, script string, gateFD uintptr,
) *supervisorCovGuardian {
	t.Helper()
	supervisorCovRequireRoot(t)
	restoreTurnSupervisorSeams(t)

	// The liveness helper's start gate arrives as descriptor 10, and a POSIX
	// shell cannot name a two-digit descriptor in a redirection.
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("liveness stub requires a shell that can redirect two-digit descriptors")
	}

	deadline := turnSupervisorReadDeadline
	t.Cleanup(func() { turnSupervisorReadDeadline = deadline })

	control, controlWrite := supervisorCovPipe(t)
	startRead, startWrite := supervisorCovPipe(t)
	completionRead, completionWrite := supervisorCovPipe(t)

	turnSupervisorEffectiveUID = func() int { return 0 }
	turnSupervisorEnable = func() error { return nil }
	turnSupervisorAcquireStandalone = func(
		uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
	) (*agentStandaloneIdentity, error) {
		return supervisorCovStandalone(t), nil
	}
	opened := make([]uintptr, 0, 2)
	turnSupervisorOpenFile = func(fd uintptr, _ string) *os.File {
		opened = append(opened, fd)

		switch fd {
		case 6:
			return completionWrite
		case gateFD:
			return startRead
		default:
			return nil
		}
	}
	turnSupervisorExecutable = func() (string, error) { return shell, nil }
	turnSupervisorCommand = func(string, ...string) *exec.Cmd {
		return exec.Command(shell, "-c", script)
	}

	contained := make(chan [2]int, 8)
	turnSupervisorContain = func(supervisor, native int) error {
		contained <- [2]int{supervisor, native}

		return nil
	}

	notified := make(chan chan<- os.Signal, 1)
	turnSupervisorSignalNotify = func(target chan<- os.Signal, _ ...os.Signal) { notified <- target }
	turnSupervisorSignalStop = func(chan<- os.Signal) {}

	return &supervisorCovGuardian{
		config:       supervisorCovEncode(t, config),
		control:      control,
		controlWrite: controlWrite,
		startGate:    startWrite,
		completion:   completionRead,
		ready:        supervisorCovNewPublisher(),
		notified:     notified,
		contained:    contained,
		opened:       &opened,
	}
}

func (guardian *supervisorCovGuardian) start(t *testing.T) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- runTurnSupervisorGuardian(guardian.config, guardian.control, guardian.ready)
	}()

	return done
}

// releaseOnArmed opens the start gate as soon as the guardian republishes the
// armed state, which is exactly what the parent does.
func (guardian *supervisorCovGuardian) releaseOnArmed(t *testing.T) {
	t.Helper()

	stop := make(chan struct{})
	released := make(chan struct{})

	t.Cleanup(func() {
		close(stop)
		<-released
	})

	go func() {
		defer close(released)

		select {
		case <-guardian.ready.armed:
			_, _ = guardian.startGate.Write([]byte{1})
			_ = guardian.startGate.Close()
		case <-stop:
		}
	}()
}

// proof reports what the guardian published on its completion descriptor. The
// guardian closes that descriptor as it returns, so this must be read after the
// run has finished.
func (guardian *supervisorCovGuardian) proof(t *testing.T) string {
	t.Helper()

	payload, err := io.ReadAll(guardian.completion)
	require.NoError(t, err)

	return string(payload)
}

func (guardian *supervisorCovGuardian) await(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("guardian never returned, published %q", guardian.ready.lines())

		return nil
	}
}

// supervisorCovLivenessScript renders a liveness stub. Each stage is optional so
// a scenario can stop the handshake exactly where it wants to.
func supervisorCovLivenessScript(stages ...string) string {
	return strings.Join(stages, "\n") + "\n"
}

var (
	supervisorCovArm       = "printf 'armed\\n' >&" + supervisorCovLivenessData
	supervisorCovAwaitGate = "cat <&" + supervisorCovLivenessStartGate + " >/dev/null"
	supervisorCovReport    = "printf 'ready:%d\\n' \"$$\" >&" + supervisorCovLivenessData
	supervisorCovAwaitTurn = "cat <&" + supervisorCovLivenessControl + " >/dev/null"
	supervisorCovDone      = "printf 'done\\n' >&" + supervisorCovLivenessData
	supervisorCovLinger    = "sleep 30"
)

// TestRunTurnSupervisorGuardianRefusesAnIncompleteInheritance proves the
// guardian will not begin a turn on a descriptor set it cannot account for, and
// that once it can report at all it reports containment for every refusal made
// before anything was launched. The parent waits on that proof; a guardian that
// exited silently would leave the parent holding a turn that never started.
func TestRunTurnSupervisorGuardianRefusesAnIncompleteInheritance(t *testing.T) {
	t.Run("no completion descriptor", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")
		turnSupervisorOpenFile = func(uintptr, string) *os.File { return nil }

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "Claude guardian completion descriptor is unavailable")
	})

	t.Run("completion descriptor cannot be sealed", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		want := errors.New("seal completion")
		turnSupervisorFcntl = func(uintptr, int, int) (int, error) { return 0, want }

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
	})

	t.Run("control is not an inheritable file", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		done := make(chan error, 1)
		go func() {
			done <- runTurnSupervisorGuardian(fixture.config, strings.NewReader(""), fixture.ready)
		}()

		err := fixture.await(t, done)
		require.ErrorContains(t, err, "Claude guardian control input is not an inheritable file")
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("config cannot be decoded", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")
		fixture.config = strings.NewReader("not json")

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "decode Claude guardian config")
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("config is refused", func(t *testing.T) {
		config := supervisorCovConfig()
		config.AuthorityOrigin = "adopted"
		fixture := supervisorCovGuardianFixture(t, config, "")

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "authority origin is invalid")
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("borrowed start gate is unavailable", func(t *testing.T) {
		// A borrowed guardian inherits two extra authority descriptors, which
		// pushes its start gate from 7 to 9. Reading the standalone number
		// would make it wait on an authority descriptor instead of the gate.
		config := supervisorCovConfig()
		config.AuthorityOrigin = turnSupervisorBorrowed
		config.IdentityLock = true
		config.AuthorityDomain = true
		config.Isolation.StandaloneOwnerID = ""
		config.Isolation.StandaloneStateRoot = ""

		fixture := supervisorCovGuardianFixtureAt(t, config, "", 7)

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "Claude guardian start gate is unavailable")
		require.Equal(t, []uintptr{6, 9}, *fixture.opened)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("start gate cannot be sealed", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		want := errors.New("seal start gate")
		calls := 0
		turnSupervisorFcntl = func(uintptr, int, int) (int, error) {
			calls++
			if calls == 3 {
				return 0, want
			}

			return 0, nil
		}

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})
}

// TestRunTurnSupervisorGuardianAbandonsTheTurnBeforeItArms proves the guardian
// contains and reports rather than proceeding when it cannot take the turn's
// authority, cannot restrict its own privileges, or cannot launch its liveness
// helper. Each of these happens before anything is running under the target
// identity, so the correct outcome is a contained turn the parent can retire.
func TestRunTurnSupervisorGuardianAbandonsTheTurnBeforeItArms(t *testing.T) {
	t.Run("authority cannot be taken", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		want := errors.New("claim")
		turnSupervisorAcquireStandalone = func(
			uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			return nil, want
		}

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("privileges cannot be restricted", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		want := errors.New("subreaper")
		turnSupervisorEnable = func() error { return want }

		err := fixture.await(t, fixture.start(t))
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "enable Claude guardian privileges")
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("liveness helper cannot be launched", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), "")

		want := errors.New("resolve executable")
		turnSupervisorExecutable = func() (string, error) { return "", want }

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
		require.Equal(t, [2]int{turnSupervisorProcessID(), 0}, <-fixture.contained)
	})

	t.Run("wait on the liveness helper cannot be bounded", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(), supervisorCovLivenessScript(supervisorCovLinger),
		)

		want := errors.New("arm deadline")
		turnSupervisorReadDeadline = func(*os.File, time.Time) error { return want }

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
		require.Equal(t, [2]int{turnSupervisorProcessID(), 0}, <-fixture.contained)
	})
}

// TestRunTurnSupervisorGuardianRefusesEveryArmingDeviation proves the guardian
// only republishes the armed state its helper actually reported, and only opens
// the native launch once the parent has released the gate. Everything between
// the helper's first line and the native launch is a two-party handshake; a
// guardian that skipped a step would let the tree start with either party
// unprepared.
func TestRunTurnSupervisorGuardianRefusesEveryArmingDeviation(t *testing.T) {
	t.Run("armed state never arrives", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(t, supervisorCovConfig(), supervisorCovLivenessScript(":"))

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "await Claude liveness readiness")
		require.Empty(t, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("armed state is unrecognised", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript("printf 'started\\n' >&"+supervisorCovLivenessData, supervisorCovLinger),
		)

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, `invalid Claude liveness armed state "started"`)
		require.Empty(t, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("armed state cannot be republished", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(supervisorCovArm, supervisorCovLinger),
		)

		want := errors.New("publish armed")
		fixture.ready.refuse = turnSupervisorArmed
		fixture.ready.err = want

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("parent never releases the start gate", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport),
		)
		require.NoError(t, fixture.startGate.Close())

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "await Claude guardian start gate")
		require.Equal(t, []string{turnSupervisorArmed}, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("liveness helper released its own gate first", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "gate-released")
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm,
				"exec "+supervisorCovLivenessStartGate+"<&-",
				": > "+marker,
				supervisorCovLinger,
			),
		)

		done := fixture.start(t)
		fixture.ready.await(t, fixture.ready.armed, "armed state")
		supervisorCovAwaitFile(t, marker)

		_, err := fixture.startGate.Write([]byte{1})
		require.NoError(t, err)

		err = fixture.await(t, done)
		require.ErrorContains(t, err, "release Claude liveness start gate")
		require.ErrorIs(t, err, syscall.EPIPE)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("readiness never arrives", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(supervisorCovArm, supervisorCovAwaitGate),
		)
		fixture.releaseOnArmed(t)

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "await Claude liveness readiness")
		require.Equal(t, []string{turnSupervisorArmed}, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("wait on the running turn cannot be unbounded", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport, supervisorCovLinger,
			),
		)
		fixture.releaseOnArmed(t)

		want := errors.New("unbound deadline")
		baseline := turnSupervisorReadDeadline
		calls := 0
		turnSupervisorReadDeadline = func(file *os.File, when time.Time) error {
			calls++
			if calls == 2 {
				return want
			}

			return baseline(file, when)
		}

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("readiness is malformed", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate,
				"printf 'ready:none\\n' >&"+supervisorCovLivenessData, supervisorCovLinger,
			),
		)
		fixture.releaseOnArmed(t)

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, `invalid Claude liveness native pid "none"`)
		require.Equal(t, []string{turnSupervisorArmed}, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("readiness cannot be republished", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport, supervisorCovLinger,
			),
		)
		fixture.releaseOnArmed(t)

		want := errors.New("publish readiness")
		fixture.ready.refuse = turnSupervisorReady
		fixture.ready.err = want

		require.ErrorIs(t, fixture.await(t, fixture.start(t)), want)
		require.Equal(t, turnSupervisorProof, fixture.proof(t))

		// The native pid the helper reported is the group containment must
		// address; a readiness the parent never saw still has to be cleaned up
		// against that pid rather than against nothing.
		require.NotZero(t, (<-fixture.contained)[1])
	})
}

func supervisorCovAwaitFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("liveness stub never produced %q", path)
}

// TestRunTurnSupervisorGuardianRetiresTheTurnOnItsTerminalReport proves the
// guardian distinguishes the three ways a running turn can end. Only a helper
// that reports "done" retires the turn cleanly; a helper that reports a failure
// and a helper that dies without reporting are both contained, and the guardian
// publishes its proof from what it observed rather than from the helper's word.
func TestRunTurnSupervisorGuardianRetiresTheTurnOnItsTerminalReport(t *testing.T) {
	t.Run("liveness helper reports done", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport, supervisorCovDone,
			),
		)
		fixture.releaseOnArmed(t)

		require.NoError(t, fixture.await(t, fixture.start(t)))
		require.Equal(t, []string{turnSupervisorArmed, turnSupervisorReady}, fixture.ready.lines())
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
		require.Empty(t, fixture.contained, "a cleanly reported turn was contained by force")
	})

	t.Run("control channel ends the turn", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport,
				supervisorCovAwaitTurn, supervisorCovDone,
			),
		)
		fixture.releaseOnArmed(t)

		done := fixture.start(t)
		fixture.ready.await(t, fixture.ready.ready, "readiness")
		require.NoError(t, fixture.controlWrite.Close())

		require.NoError(t, fixture.await(t, done))
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
	})

	t.Run("liveness helper reports a failure", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport,
				"printf 'error:liveness gave up\\n' >&"+supervisorCovLivenessData,
			),
		)
		fixture.releaseOnArmed(t)

		err := fixture.await(t, fixture.start(t))
		require.ErrorContains(t, err, "Claude liveness completion failed: liveness gave up")

		// A helper that reported a failure was never contained by itself, so
		// the guardian must not publish the proof that retires the identity.
		require.Empty(t, fixture.proof(t))
		require.NotZero(t, (<-fixture.contained)[1])
	})

	t.Run("forwarded signal ends the turn without a report", func(t *testing.T) {
		fixture := supervisorCovGuardianFixture(
			t, supervisorCovConfig(),
			supervisorCovLivenessScript(
				supervisorCovArm, supervisorCovAwaitGate, supervisorCovReport, supervisorCovLinger,
			),
		)
		fixture.releaseOnArmed(t)

		done := fixture.start(t)
		target := <-fixture.notified
		fixture.ready.await(t, fixture.ready.ready, "readiness")

		// A signal with no kernel number cannot be forwarded, so it is dropped
		// rather than turned into signal zero, which probes a group instead of
		// terminating it.
		target <- supervisorTestSignal("not-a-kernel-signal")
		target <- syscall.SIGKILL

		err := fixture.await(t, done)
		require.ErrorContains(t, err, "Claude liveness exited without completion report")

		// The helper died from the guardian's own forwarded signal and the tree
		// was contained, so the turn is provably over even without a report.
		require.Equal(t, turnSupervisorProof, fixture.proof(t))
		require.NotZero(t, (<-fixture.contained)[1])
	})
}
