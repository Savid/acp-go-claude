//go:build linux

package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// supervisorCovPublisher records everything a supervisor publishes on one of
// its streams and can refuse a chosen line. The supervisor writes from its own
// goroutine, so recording is what lets a test read the publication order back
// without racing it.
type supervisorCovPublisher struct {
	mu      sync.Mutex
	written []string
	refuse  string
	err     error
	armed   chan struct{}
	ready   chan struct{}
	armOnce sync.Once
	rdyOnce sync.Once
}

func supervisorCovNewPublisher() *supervisorCovPublisher {
	return &supervisorCovPublisher{armed: make(chan struct{}), ready: make(chan struct{})}
}

func (publisher *supervisorCovPublisher) Write(value []byte) (int, error) {
	text := string(value)

	publisher.mu.Lock()
	publisher.written = append(publisher.written, text)
	refuse, refusal := publisher.refuse, publisher.err
	publisher.mu.Unlock()

	if refuse != "" && strings.HasPrefix(text, refuse) {
		return 0, refusal
	}

	switch {
	case text == turnSupervisorArmed:
		publisher.armOnce.Do(func() { close(publisher.armed) })
	case strings.HasPrefix(text, "ready:"), text == turnSupervisorReady:
		publisher.rdyOnce.Do(func() { close(publisher.ready) })
	}

	return len(value), nil
}

func (publisher *supervisorCovPublisher) lines() []string {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	return append([]string(nil), publisher.written...)
}

func (publisher *supervisorCovPublisher) await(t *testing.T, gate <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-gate:
	case <-time.After(10 * time.Second):
		t.Fatalf("supervisor never published %s, published %q", what, publisher.lines())
	}
}

// supervisorCovStandalone returns a standalone authority handle over two real
// files, which is what a claimed identity looks like to everything downstream
// of the claim itself.
func supervisorCovStandalone(t *testing.T) *agentStandaloneIdentity {
	t.Helper()

	identity, err := os.CreateTemp(t.TempDir(), "identity")
	require.NoError(t, err)
	domain, err := os.CreateTemp(t.TempDir(), "domain")
	require.NoError(t, err)

	return &agentStandaloneIdentity{
		identity: &agentIdentityLock{file: identity}, authority: &agentIdentityLock{file: domain},
	}
}

func supervisorCovEncode(t *testing.T, config turnSupervisorConfig) io.Reader {
	t.Helper()

	payload, err := json.Marshal(config)
	require.NoError(t, err)

	return bytes.NewReader(payload)
}

// supervisorCovNativeSeams installs the seams every native-body case shares and
// returns a channel that yields the signal channel the body registered.
func supervisorCovNativeSeams(t *testing.T) chan chan<- os.Signal {
	t.Helper()
	supervisorCovRequireRoot(t)
	restoreTurnSupervisorSeams(t)

	turnSupervisorEffectiveUID = func() int { return 0 }
	turnSupervisorCoreLimit = func() error { return nil }
	turnSupervisorNoNewPrivs = func() error { return nil }
	turnSupervisorEnable = func() error { return nil }
	turnSupervisorContain = func(int, int) error { return nil }
	turnSupervisorAcquireStandalone = func(
		uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
	) (*agentStandaloneIdentity, error) {
		return supervisorCovStandalone(t), nil
	}

	notified := make(chan chan<- os.Signal, 1)
	turnSupervisorSignalNotify = func(target chan<- os.Signal, _ ...os.Signal) { notified <- target }
	turnSupervisorSignalStop = func(chan<- os.Signal) {}

	return notified
}

// supervisorCovNativeConfig returns a valid standalone native config running
// the supplied command under the dropped identity.
func supervisorCovNativeConfig(path string, args ...string) turnSupervisorConfig {
	config := supervisorCovConfig()
	config.Path = path
	config.Args = append([]string{path}, args...)

	return config
}

// TestRunTurnSupervisorNativeRefusesEveryUnusableAuthority proves the native
// body claims exactly one complete authority before it launches anything. It is
// the last process that holds the identity leases for the turn, so a refused
// config, a borrowed adoption it cannot make, a standalone claim it cannot take,
// and a claim that came back missing a lease all have to abandon the launch.
func TestRunTurnSupervisorNativeRefusesEveryUnusableAuthority(t *testing.T) {
	t.Run("config is refused", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		config := supervisorCovConfig()
		config.AuthorityOrigin = "adopted"

		err := runTurnSupervisorNative(
			supervisorCovEncode(t, config), nil, nil, strings.NewReader("\x01"),
			io.Discard, io.Discard, 6, 7,
		)
		require.ErrorContains(t, err, "authority origin is invalid")
	})

	t.Run("borrowed adoption fails", func(t *testing.T) {
		supervisorCovNativeSeams(t)
		turnSupervisorOpenFile = func(uintptr, string) *os.File { return nil }

		config := supervisorCovConfig()
		config.AuthorityOrigin = turnSupervisorBorrowed
		config.IdentityLock = true
		config.AuthorityDomain = true
		config.Isolation.StandaloneOwnerID = ""
		config.Isolation.StandaloneStateRoot = ""

		err := runTurnSupervisorNative(
			supervisorCovEncode(t, config), nil, nil, strings.NewReader("\x01"),
			io.Discard, io.Discard, 6, 7,
		)
		require.ErrorContains(t, err, "adopt Claude agent identity lock")
	})

	t.Run("standalone claim fails", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		want := errors.New("claim")
		turnSupervisorAcquireStandalone = func(
			uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			return nil, want
		}

		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovConfig()), nil, nil, strings.NewReader("\x01"),
			io.Discard, io.Discard, 6, 7,
		)
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "acquire Claude standalone agent identity authority")
	})

	t.Run("claimed authority is incomplete", func(t *testing.T) {
		supervisorCovNativeSeams(t)
		turnSupervisorAcquireStandalone = func(
			uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal,
		) (*agentStandaloneIdentity, error) {
			return &agentStandaloneIdentity{}, nil
		}

		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovConfig()), nil, nil, strings.NewReader("\x01"),
			io.Discard, io.Discard, 6, 7,
		)
		require.ErrorContains(t, err, "Claude agent identity authority is incomplete")
	})
}

// TestRunTurnSupervisorNativeRefusesToLaunchWithoutItsGuardianAndGate proves
// nothing is launched until the guardian is proven alive and the parent has
// released the start gate. The guardian holds the containment hold for the whole
// turn, so a native root started after it died — or before the parent opened the
// gate — would run with nothing accountable for it.
func TestRunTurnSupervisorNativeRefusesToLaunchWithoutItsGuardianAndGate(t *testing.T) {
	t.Run("guardian already gone", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		peer, peerWrite := supervisorCovPipe(t)
		require.NoError(t, peerWrite.Close())

		contained := make(chan [2]int, 4)
		turnSupervisorContain = func(supervisor, native int) error {
			contained <- [2]int{supervisor, native}

			return nil
		}

		control, _ := supervisorCovPipe(t)
		ready := supervisorCovNewPublisher()
		completion := supervisorCovNewPublisher()
		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/bin/true")),
			[]io.Reader{control}, peer, strings.NewReader("\x01"),
			ready, completion, 6, 7,
		)
		require.ErrorIs(t, err, errTurnSupervisorGuardianExited)
		require.Empty(t, ready.lines(), "the native body armed after its guardian died")
		require.Equal(t, []string{turnSupervisorProof}, completion.lines())
		require.Equal(t, [2]int{turnSupervisorProcessID(), 0}, <-contained)
	})

	t.Run("armed state cannot be published", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		ready := supervisorCovNewPublisher()
		ready.refuse = turnSupervisorArmed
		ready.err = errors.New("publish armed")

		control, _ := supervisorCovPipe(t)
		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/bin/true")),
			[]io.Reader{control}, nil, strings.NewReader("\x01"),
			ready, io.Discard, 6, 7,
		)
		require.ErrorIs(t, err, ready.err)
		require.ErrorContains(t, err, "publish Claude liveness armed state")
	})

	t.Run("start gate closed before release", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		control, _ := supervisorCovPipe(t)
		ready := supervisorCovNewPublisher()
		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/bin/true")),
			[]io.Reader{control}, nil, strings.NewReader(""),
			ready, io.Discard, 6, 7,
		)
		require.ErrorContains(t, err, "Claude guardian start gate closed before native launch")
		require.NotContains(
			t, strings.Join(ready.lines(), ""), "ready:",
			"a native root was announced without the parent releasing the start gate",
		)
	})

	t.Run("privilege bootstrap fails", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		want := errors.New("core limit")
		turnSupervisorCoreLimit = func() error { return want }

		control, _ := supervisorCovPipe(t)
		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/bin/true")),
			[]io.Reader{control}, nil, strings.NewReader("\x01"),
			supervisorCovNewPublisher(), io.Discard, 6, 7,
		)
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "enable Claude native supervisor privileges")
	})

	t.Run("native root cannot start", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		control, _ := supervisorCovPipe(t)
		err := runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/nonexistent/claude")),
			[]io.Reader{control}, nil, strings.NewReader("\x01"),
			supervisorCovNewPublisher(), io.Discard, 6, 7,
		)
		require.ErrorContains(t, err, "start supervised Claude native root")
	})
}

// TestRunTurnSupervisorNativeContainsWhatItCannotReport proves a native root
// whose readiness the parent will never see is killed and contained rather than
// left running. The parent only learns the native pid from that line, so a tree
// it never heard about is a tree nobody can reap.
func TestRunTurnSupervisorNativeContainsWhatItCannotReport(t *testing.T) {
	supervisorCovNativeSeams(t)

	contained := make(chan [2]int, 4)
	turnSupervisorContain = func(supervisor, native int) error {
		contained <- [2]int{supervisor, native}

		return nil
	}

	signaled := make(chan [2]int, 4)
	turnSupervisorSignalGroup = func(pgid int, processSignal syscall.Signal) error {
		signaled <- [2]int{pgid, int(processSignal)}

		return signalProcessGroupID(pgid, processSignal)
	}

	ready := supervisorCovNewPublisher()
	ready.refuse = "ready:"
	ready.err = errors.New("publish readiness")

	control, _ := supervisorCovPipe(t)
	err := runTurnSupervisorNative(
		supervisorCovEncode(t, supervisorCovNativeConfig("/bin/sleep", "60")),
		[]io.Reader{control}, nil, strings.NewReader("\x01"),
		ready, io.Discard, 6, 7,
	)
	require.ErrorIs(t, err, ready.err)
	require.ErrorContains(t, err, "publish Claude native supervisor readiness")

	killed := <-signaled
	require.Equal(t, int(syscall.SIGKILL), killed[1])
	require.Positive(t, killed[0])

	proof := <-contained
	require.Equal(t, turnSupervisorProcessID(), proof[0])
	require.Equal(t, killed[0], proof[1], "containment addressed a different tree than the one killed")
}

// TestRunTurnSupervisorNativeEndsTheTurnOnEveryTerminalEvent proves the native
// body leaves its wait loop for each of the three things that can end a turn,
// and that it only reports the turn done once the tree is contained. A native
// root that exits, a control channel that closes, and a forwarded signal are
// different events with the same obligation.
func TestRunTurnSupervisorNativeEndsTheTurnOnEveryTerminalEvent(t *testing.T) {
	t.Run("native root exits", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		contained := make(chan [2]int, 4)
		turnSupervisorContain = func(supervisor, native int) error {
			contained <- [2]int{supervisor, native}

			return nil
		}

		control, _ := supervisorCovPipe(t)
		ready := supervisorCovNewPublisher()
		completion := supervisorCovNewPublisher()
		require.NoError(t, runTurnSupervisorNative(
			supervisorCovEncode(t, supervisorCovNativeConfig("/bin/true")),
			[]io.Reader{control}, nil, strings.NewReader("\x01"),
			ready, completion, 6, 7,
		))

		lines := ready.lines()
		require.Equal(t, turnSupervisorArmed, lines[0])
		require.True(t, strings.HasPrefix(lines[1], "ready:"))
		require.Equal(t, "done\n", lines[2], "the turn was reported without a terminal result")
		require.Empty(t, completion.lines(), "a paired liveness published the guardian's own proof")
		require.NotZero(t, (<-contained)[1])
	})

	t.Run("control channel closes", func(t *testing.T) {
		supervisorCovNativeSeams(t)

		signaled := make(chan [2]int, 4)
		turnSupervisorSignalGroup = func(pgid int, processSignal syscall.Signal) error {
			signaled <- [2]int{pgid, int(processSignal)}

			return signalProcessGroupID(pgid, processSignal)
		}

		control, controlWrite := supervisorCovPipe(t)
		ready := supervisorCovNewPublisher()

		done := make(chan error, 1)
		go func() {
			done <- runTurnSupervisorNative(
				supervisorCovEncode(t, supervisorCovNativeConfig("/bin/sleep", "60")),
				[]io.Reader{control}, nil, strings.NewReader("\x01"),
				ready, io.Discard, 6, 7,
			)
		}()

		ready.await(t, ready.ready, "native readiness")
		require.NoError(t, controlWrite.Close())
		require.Error(t, <-done, "a SIGKILLed native root reported a clean exit")

		killed := <-signaled
		require.Equal(t, int(syscall.SIGKILL), killed[1])
		require.Equal(t, "done\n", ready.lines()[2])
	})

	t.Run("signals are forwarded to the native group", func(t *testing.T) {
		notified := supervisorCovNativeSeams(t)

		signaled := make(chan [2]int, 8)
		turnSupervisorSignalGroup = func(pgid int, processSignal syscall.Signal) error {
			signaled <- [2]int{pgid, int(processSignal)}

			return signalProcessGroupID(pgid, processSignal)
		}

		control, controlWrite := supervisorCovPipe(t)
		ready := supervisorCovNewPublisher()

		done := make(chan error, 1)
		go func() {
			done <- runTurnSupervisorNative(
				supervisorCovEncode(t, supervisorCovNativeConfig("/bin/sleep", "60")),
				[]io.Reader{control}, nil, strings.NewReader("\x01"),
				ready, io.Discard, 6, 7,
			)
		}()

		target := <-notified
		ready.await(t, ready.ready, "native readiness")

		// A signal that is not a kernel signal has no number to forward, so it
		// must be dropped rather than turned into signal zero — which probes a
		// process group instead of terminating it.
		target <- supervisorTestSignal("not-a-kernel-signal")
		target <- syscall.SIGTERM

		forwarded := <-signaled
		require.Equal(t, int(syscall.SIGTERM), forwarded[1])

		require.NoError(t, controlWrite.Close())
		require.Error(t, <-done)
	})
}

// TestRunTurnSupervisorNativeNeverReportsAnUncontainedTree proves an
// incomplete containment is surfaced instead of being reported as a finished
// turn, whichever event ended it. The deferred completion publishes the turn's
// terminal result, so returning the wait error over a failed containment would
// tell the parent a tree it cannot account for is gone.
func TestRunTurnSupervisorNativeNeverReportsAnUncontainedTree(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		command []string
		close   bool
	}{
		{name: "after the native root exits", command: []string{"/bin/true"}},
		{name: "after the control channel closes", command: []string{"/bin/sleep", "60"}, close: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			supervisorCovNativeSeams(t)

			want := errors.New("contain")
			turnSupervisorContain = func(int, int) error { return want }

			control, controlWrite := supervisorCovPipe(t)
			ready := supervisorCovNewPublisher()
			completion := supervisorCovNewPublisher()

			done := make(chan error, 1)
			go func() {
				done <- runTurnSupervisorNative(
					supervisorCovEncode(t, supervisorCovNativeConfig(testCase.command[0], testCase.command[1:]...)),
					[]io.Reader{control}, nil, strings.NewReader("\x01"),
					ready, completion, 6, 7,
				)
			}()

			ready.await(t, ready.ready, "native readiness")

			if testCase.close {
				require.NoError(t, controlWrite.Close())
			}

			require.ErrorIs(t, <-done, want)
			require.NotContains(
				t, strings.Join(ready.lines(), ""), "done\n",
				"an uncontained turn was reported as finished",
			)
			require.Empty(t, completion.lines())
		})
	}
}
