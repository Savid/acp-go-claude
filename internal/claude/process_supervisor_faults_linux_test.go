//go:build linux

package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// supervisorCovWriteFault is a writer that refuses every publication, which is
// how these tests observe what the supervisor does when it cannot report.
type supervisorCovWriteFault struct {
	err    error
	writes int
}

func (fault *supervisorCovWriteFault) Write(value []byte) (int, error) {
	fault.writes++

	return 0, fault.err
}

func supervisorCovRequireRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("trusted supervisor credential boundary requires root")
	}
}

// supervisorCovIsolation returns the standalone isolation shape the supervisor
// accepts, with an identity that differs from the trusted root.
func supervisorCovIsolation() *ProcessIsolation {
	return &ProcessIsolation{
		UID: 64251, GID: 64252,
		BaseEnvironment:     map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID:   "claude-supervisor-cov",
		StandaloneStateRoot: "/var/lib/acp-go-claude-cov",
	}
}

// supervisorCovConfig returns the standalone guardian config shape that passes
// validation, so a test can invalidate exactly one field at a time.
func supervisorCovConfig() turnSupervisorConfig {
	isolation := supervisorCovIsolation()

	return turnSupervisorConfig{
		Path:            "/bin/true",
		Args:            []string{"true"},
		Env:             []string{"PATH=/usr/bin:/bin"},
		Isolation:       *isolation,
		AuthorityOrigin: turnSupervisorStandalone,
	}
}

// supervisorCovPipe returns a pipe whose ends are both released when the test
// ends, so a supervisor goroutine draining one of them cannot outlive the test.
func supervisorCovPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	read, write, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = read.Close()
		_ = write.Close()
	})

	return read, write
}

// TestTurnSupervisorIdentityRuleRefusesEveryNonTrustedShape proves the identity
// precondition every supervisor entry point shares. Isolation must exist,
// the wrapper must already hold the trusted root identity — it drops privilege
// for the native root and can never regain it — and the native target must not
// be that same root, or the supervisor would be handing the tree back the
// authority it exists to remove.
func TestTurnSupervisorIdentityRuleRefusesEveryNonTrustedShape(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	require.ErrorContains(t, validateTurnSupervisorIdentity(nil), "process isolation is required")

	turnSupervisorEffectiveUID = func() int { return 1000 }
	require.ErrorContains(
		t,
		validateTurnSupervisorIdentity(supervisorCovIsolation()),
		"trusted root identity is required, effective uid is 1000",
	)

	turnSupervisorEffectiveUID = func() int { return 0 }
	require.ErrorContains(
		t,
		validateTurnSupervisorIdentity(&ProcessIsolation{UID: 0, GID: 0}),
		"native target identity must differ from the trusted supervisor",
	)
	require.NoError(t, validateTurnSupervisorIdentity(supervisorCovIsolation()))
}

// TestTurnSupervisorConfigRefusesEveryInvalidAuthorityDisposition proves the
// guardian re-validates the config it decodes rather than trusting the parent
// that sealed it. The config names the command to run and the authority to run
// it under, so each refusal here is the boundary between the two authority
// origins: a borrowed config must carry a lock and no standalone fields, a
// standalone config must carry both standalone fields, and no other origin
// exists.
func TestTurnSupervisorConfigRefusesEveryInvalidAuthorityDisposition(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorEffectiveUID = func() int { return 0 }

	for _, testCase := range []struct {
		name   string
		mutate func(*turnSupervisorConfig)
		want   string
	}{
		{
			name:   "no command",
			mutate: func(config *turnSupervisorConfig) { config.Path = "" },
			want:   "claude native supervisor config is incomplete",
		},
		{
			name:   "no argv",
			mutate: func(config *turnSupervisorConfig) { config.Args = nil },
			want:   "claude native supervisor config is incomplete",
		},
		{
			name:   "lock without domain",
			mutate: func(config *turnSupervisorConfig) { config.IdentityLock = true },
			want:   "identity lock and authority domain must be provided together",
		},
		{
			name: "borrowed without a lock",
			mutate: func(config *turnSupervisorConfig) {
				config.AuthorityOrigin = turnSupervisorBorrowed
				config.Isolation.StandaloneOwnerID = ""
				config.Isolation.StandaloneStateRoot = ""
			},
			want: "borrowed supervisor authority disposition is invalid",
		},
		{
			name: "borrowed carrying standalone owner fields",
			mutate: func(config *turnSupervisorConfig) {
				config.AuthorityOrigin = turnSupervisorBorrowed
				config.IdentityLock = true
				config.AuthorityDomain = true
			},
			want: "borrowed supervisor authority disposition is invalid",
		},
		{
			name:   "standalone without an owner id",
			mutate: func(config *turnSupervisorConfig) { config.Isolation.StandaloneOwnerID = "" },
			want:   "standalone supervisor authority disposition is invalid",
		},
		{
			name:   "standalone without a state root",
			mutate: func(config *turnSupervisorConfig) { config.Isolation.StandaloneStateRoot = "" },
			want:   "standalone supervisor authority disposition is invalid",
		},
		{
			name:   "unknown authority origin",
			mutate: func(config *turnSupervisorConfig) { config.AuthorityOrigin = "adopted" },
			want:   "authority origin is invalid",
		},
		{
			name:   "isolation the wrapper would refuse",
			mutate: func(config *turnSupervisorConfig) { config.Isolation.BaseEnvironment = nil },
			want:   "validate Claude native supervisor isolation",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := supervisorCovConfig()
			testCase.mutate(&config)
			require.ErrorContains(t, validateTurnSupervisorConfig(config), testCase.want)
		})
	}

	require.NoError(t, validateTurnSupervisorConfig(supervisorCovConfig()))

	// A borrowed config carries its lock over an inherited descriptor rather
	// than in the config, so validation substitutes a placeholder capability.
	// Without it the borrowed shape would be refused as an incomplete
	// standalone one.
	borrowed := supervisorCovConfig()
	borrowed.AuthorityOrigin = turnSupervisorBorrowed
	borrowed.IdentityLock = true
	borrowed.AuthorityDomain = true
	borrowed.Isolation.StandaloneOwnerID = ""
	borrowed.Isolation.StandaloneStateRoot = ""
	require.NoError(t, validateTurnSupervisorConfig(borrowed))

	turnSupervisorEffectiveUID = func() int { return 1000 }
	require.ErrorContains(
		t,
		validateTurnSupervisorConfig(supervisorCovConfig()),
		"validate Claude native supervisor identity",
	)
}

// TestTurnSupervisorPrivilegeEnableOrdersAndFailsClosed proves the supervisor's
// privilege bootstrap runs in the one order that works and abandons the launch
// on any step. Core dumps go first because a dump of the supervisor would
// expose the authority descriptors it holds, the subreaper claim has to precede
// the launch or descendants would reparent past it, and no-new-privs goes last
// because it is irreversible and would block nothing that comes before it.
func TestTurnSupervisorPrivilegeEnableOrdersAndFailsClosed(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	var options []int
	turnSupervisorSetrlimit = func(int, *unix.Rlimit) error { return nil }
	turnSupervisorPrctl = func(option int, _, _, _, _ uintptr) error {
		options = append(options, option)

		return nil
	}
	require.NoError(t, enableTurnSupervisor())
	require.Equal(
		t,
		[]int{unix.PR_SET_CHILD_SUBREAPER, unix.PR_SET_DUMPABLE, unix.PR_SET_NO_NEW_PRIVS},
		options,
	)

	wantLimit := errors.New("core limit")
	turnSupervisorSetrlimit = func(int, *unix.Rlimit) error { return wantLimit }
	options = nil
	err := enableTurnSupervisor()
	require.ErrorIs(t, err, wantLimit)
	require.ErrorContains(t, err, "disable Claude native core dumps")
	require.Empty(t, options, "privilege bootstrap continued past an undisabled core dump")

	turnSupervisorSetrlimit = func(int, *unix.Rlimit) error { return nil }

	for step, want := range map[int]int{1: unix.PR_SET_CHILD_SUBREAPER, 2: unix.PR_SET_DUMPABLE} {
		wantErr := errors.New("prctl")
		calls := 0
		options = nil
		turnSupervisorPrctl = func(option int, _, _, _, _ uintptr) error {
			calls++
			options = append(options, option)
			if calls == step {
				return wantErr
			}

			return nil
		}
		require.ErrorIs(t, enableTurnSupervisor(), wantErr)
		require.Equal(t, want, options[len(options)-1])
		require.Len(t, options, step, "privilege bootstrap continued past a refused step")
	}
}

// TestTurnSupervisorPrivilegeStepsApplyToTheCallingProcess proves the two
// standalone privilege steps really change this process rather than reporting
// success over a no-op. They run on the creator thread just before the native
// launch, so a silent no-op would hand the native root a dumpable, privilege
// escalating process.
func TestTurnSupervisorPrivilegeStepsApplyToTheCallingProcess(t *testing.T) {
	var previous unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_CORE, &previous))
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_CORE, &previous) })

	require.NoError(t, disableTurnSupervisorCoreDumps())

	var applied unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_CORE, &applied))
	require.Equal(t, uint64(0), applied.Cur)
	require.Equal(t, uint64(0), applied.Max)

	// PR_SET_NO_NEW_PRIVS is a per-thread one-way latch. Production sets it on
	// the creator thread that goes on to fork the native root, so prove it on a
	// locked thread and read it back from that same thread. The thread is
	// deliberately never unlocked: it dies with this goroutine rather than
	// returning to the scheduler with the latch set.
	proved := make(chan error, 1)

	go func() {
		runtime.LockOSThread()

		if err := enableTurnSupervisorNoNewPrivileges(); err != nil {
			proved <- err

			return
		}

		status, err := os.ReadFile("/proc/thread-self/status")
		if err != nil {
			proved <- err

			return
		}

		for _, line := range strings.Split(string(status), "\n") {
			if value, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
				if strings.TrimSpace(value) != "1" {
					proved <- fmt.Errorf("creator thread NoNewPrivs = %q", strings.TrimSpace(value))

					return
				}

				proved <- nil

				return
			}
		}

		proved <- errors.New("creator thread status does not report NoNewPrivs")
	}()

	require.NoError(t, <-proved)
}

// TestTurnSupervisorCloseOnExecIsAppliedAndFailsClosed proves the inherited
// supervisor descriptors are sealed against exec and that a descriptor whose
// flags cannot be read or written aborts the supervisor. These descriptors
// carry the config, the control channel and the completion proof; leaking any
// of them across the native exec would hand the agent the supervisor's own
// control plane.
func TestTurnSupervisorCloseOnExecIsAppliedAndFailsClosed(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	var raw [2]int
	require.NoError(t, unix.Pipe2(raw[:], 0))

	inherited := os.NewFile(uintptr(raw[0]), "inherited")
	other := os.NewFile(uintptr(raw[1]), "other")
	t.Cleanup(func() {
		_ = inherited.Close()
		_ = other.Close()
	})

	flags, err := unix.FcntlInt(inherited.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.Zero(t, flags&unix.FD_CLOEXEC, "the fixture descriptor was already sealed")

	require.NoError(t, setTurnSupervisorCloseOnExec(inherited))

	flags, err = unix.FcntlInt(inherited.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.FD_CLOEXEC)

	wantRead := errors.New("get flags")
	turnSupervisorFcntl = func(uintptr, int, int) (int, error) { return 0, wantRead }
	err = setTurnSupervisorCloseOnExec(inherited)
	require.ErrorIs(t, err, wantRead)
	require.ErrorContains(t, err, "read inherited Claude supervisor descriptor flags")

	wantSet := errors.New("set flags")
	calls := 0
	turnSupervisorFcntl = func(uintptr, int, int) (int, error) {
		calls++
		if calls == 1 {
			return 0, nil
		}

		return 0, wantSet
	}
	err = setTurnSupervisorCloseOnExec(inherited)
	require.ErrorIs(t, err, wantSet)
	require.ErrorContains(t, err, "protect inherited Claude supervisor descriptor from exec")
}

// TestInheritedTurnSupervisorInputReleasesEveryDescriptorOnFailure proves the
// supervisor entry point does not leak the descriptors it just adopted when it
// cannot seal them. A retained descriptor would survive into the failure exit
// and keep the parent's pipes open, so the parent would wait on a supervisor
// that has already given up.
func TestInheritedTurnSupervisorInputReleasesEveryDescriptorOnFailure(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	inherited := make([]*os.File, 0, 3)
	for range 3 {
		read, _ := supervisorCovPipe(t)
		inherited = append(inherited, read)
	}

	next := 0
	turnSupervisorOpenFile = func(uintptr, string) *os.File {
		file := inherited[next]
		next++

		return file
	}
	want := errors.New("seal")
	turnSupervisorFcntl = func(uintptr, int, int) (int, error) { return 0, want }

	config, control, ready, err := inheritedTurnSupervisorInput()
	require.ErrorIs(t, err, want)
	require.Nil(t, config)
	require.Nil(t, control)
	require.Nil(t, ready)

	for index, file := range inherited {
		require.Equal(
			t, ^uintptr(0), file.Fd(), "inherited descriptor %d survived the sealing failure", index,
		)
	}
}

// TestTurnSupervisorBootstrapSelectsTheLivenessRole proves the mode environment
// variable is what separates the two supervisor roles inside one binary. The
// guardian and the liveness helper inherit different descriptor layouts, so
// running the wrong body would read the wrong descriptors as config and
// control.
func TestTurnSupervisorBootstrapSelectsTheLivenessRole(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	t.Setenv(turnSupervisorModeEnv, turnSupervisorLivenessMode)

	exitCode := -1
	turnSupervisorExit = func(code int) { exitCode = code }
	turnSupervisorInput = func() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
		return io.NopCloser(strings.NewReader("config")),
			io.NopCloser(strings.NewReader("control")),
			&recordingWriteCloser{Writer: io.Discard}, nil
	}

	guardian := 0
	liveness := 0
	turnSupervisorRun = func(io.Reader, io.Reader, io.Writer) error { guardian++; return nil }
	turnSupervisorRunLiveness = func(io.Reader, io.Reader, io.Writer) error { liveness++; return nil }

	turnSupervisorBootstrap()

	require.Equal(t, 0, exitCode)
	require.Equal(t, 1, liveness)
	require.Equal(t, 0, guardian, "the liveness mode ran the guardian body")
}

// TestContainLinuxSupervisorDescendantsRefusesAnUnusableWaitResult proves an
// impossible wait result is an error rather than an empty tree. The caller
// treats a nil return as proof that every descendant is gone and exits the
// subreaper, so a wait result the supervisor cannot interpret must never be
// read as quiescence.
func TestContainLinuxSupervisorDescendantsRefusesAnUnusableWaitResult(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorSignalGroup = func(int, syscall.Signal) error { return nil }
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return -1, nil }

	descendants := 0
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) {
		descendants++

		return nil, nil
	}

	err := containLinuxSupervisorDescendants(1, 2)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorContains(t, err, "invalid supervised Claude wait result -1")
	require.Zero(t, descendants, "containment enumerated a tree it could not reap")
}

// TestReadLinuxProcessIdentityRefusesAnUnparsableProcessGroup proves an
// unparsable process group is an error rather than a zero group. Group zero
// would be handed to kill(2) as a negative pid, which addresses the caller's
// own group.
func TestReadLinuxProcessIdentityRefusesAnUnparsableProcessGroup(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	root := t.TempDir()
	turnSupervisorProcRoot = root

	fields := []string{"S", "1", "not-a-group"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}

	fields = append(fields, "10")
	writeProcStat(t, root, 9, "9 (command with spaces) "+strings.Join(fields, " "))

	identity, err := readLinuxProcessIdentity(9)
	require.ErrorContains(t, err, "parse /proc/9/stat group")
	require.Zero(t, identity.pid)
}

// TestValidateTurnSupervisorGuardianPeerReadsTheDescriptorNotOnlyTheChannel
// proves the liveness helper decides its peer is gone from the descriptor
// itself, not only from the drain goroutine's channel. The drain goroutine can
// still be scheduled when the guardian dies, so a check that trusted the
// channel alone would launch the native root with no guardian holding the
// authority.
func TestValidateTurnSupervisorGuardianPeerReadsTheDescriptorNotOnlyTheChannel(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	require.NoError(t, validateTurnSupervisorGuardianPeer(nil, nil))

	live, _ := supervisorCovPipe(t)
	open := make(chan struct{})
	require.NoError(t, validateTurnSupervisorGuardianPeer(live, open))

	// A closed write end is exactly what a dead guardian leaves behind: the
	// poll reports hangup even though the drain channel is still open.
	dead, deadWrite := supervisorCovPipe(t)
	require.NoError(t, deadWrite.Close())
	require.ErrorIs(t, validateTurnSupervisorGuardianPeer(dead, open), errTurnSupervisorGuardianExited)

	want := errors.New("poll")
	turnSupervisorPoll = func([]unix.PollFd, int) (int, error) { return 0, want }
	err := validateTurnSupervisorGuardianPeer(live, open)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "poll Claude guardian before native launch")
}

// TestTurnSupervisorAuthorityCloseReleasesWhicheverAuthorityItHolds proves the
// authority handle releases the standalone identity when it owns one and the
// borrowed pair otherwise. A standalone authority owns both locks through the
// standalone handle, so closing the pair directly would leave the handle
// believing it still holds them.
func TestTurnSupervisorAuthorityCloseReleasesWhicheverAuthorityItHolds(t *testing.T) {
	require.NoError(t, (*turnSupervisorAuthority)(nil).Close())

	identity, err := os.CreateTemp(t.TempDir(), "identity")
	require.NoError(t, err)
	domain, err := os.CreateTemp(t.TempDir(), "domain")
	require.NoError(t, err)

	standalone := &agentStandaloneIdentity{
		identity: &agentIdentityLock{file: identity}, authority: &agentIdentityLock{file: domain},
	}
	authority := &turnSupervisorAuthority{
		identity: standalone.identity, domain: standalone.authority, standalone: standalone,
	}

	require.NoError(t, authority.Close())
	require.Nil(t, standalone.identity, "the standalone handle still believes it holds its identity")
	require.Nil(t, standalone.authority)
	require.Equal(t, ^uintptr(0), identity.Fd())
	require.Equal(t, ^uintptr(0), domain.Fd())
}

// TestCompleteTurnSupervisorAuthorityRefusesToProveWhatItCannotRelease proves
// the completion proof is only published after the authority is actually
// released, and never at all when there is no authority to release. The proof
// tells the parent the identity is free for the next turn, so publishing it
// over a missing or retained authority would let the next turn contend with a
// live holder.
func TestCompleteTurnSupervisorAuthorityRefusesToProveWhatItCannotRelease(t *testing.T) {
	var completion bytes.Buffer
	require.ErrorContains(
		t,
		completeTurnSupervisorAuthority(&completion, nil, true),
		"Claude guardian authority is unavailable at completion",
	)

	var absent *turnSupervisorAuthority
	require.ErrorContains(
		t,
		completeTurnSupervisorAuthority(&completion, &absent, true),
		"Claude guardian authority is unavailable at completion",
	)
	require.Zero(t, completion.Len())

	original := agentIdentityLockClose
	t.Cleanup(func() { agentIdentityLockClose = original })
	agentIdentityLockClose = func(file *os.File) error { return file.Close() }

	identity, err := os.CreateTemp(t.TempDir(), "identity")
	require.NoError(t, err)
	domain, err := os.CreateTemp(t.TempDir(), "domain")
	require.NoError(t, err)

	authority := &turnSupervisorAuthority{
		identity: &agentIdentityLock{file: identity}, domain: &agentIdentityLock{file: domain},
	}
	want := errors.New("publish")
	fault := &supervisorCovWriteFault{err: want}
	err = completeTurnSupervisorAuthority(fault, &authority, true)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "publish Claude guardian completion")
	require.Nil(t, authority, "the authority was retained after a failed completion")
	require.Equal(t, 1, fault.writes)
}

// TestTurnSupervisorSignaledExitDistinguishesSignalsFromStatuses proves the
// guardian only treats a signalled liveness exit as a contained outcome. The
// guardian kills its own tree, so a signalled exit is its own doing; an
// ordinary non-zero status is the liveness helper giving up on its own and must
// not be reported as containment.
func TestTurnSupervisorSignaledExitDistinguishesSignalsFromStatuses(t *testing.T) {
	require.False(t, turnSupervisorSignaledExit(nil))
	require.False(t, turnSupervisorSignaledExit(errors.New("not an exit")))

	statusErr := exec.Command("/bin/sh", "-c", "exit 3").Run()
	require.Error(t, statusErr)
	require.False(t, turnSupervisorSignaledExit(statusErr))

	signaledErr := exec.Command("/bin/sh", "-c", "kill -KILL $$").Run()
	require.Error(t, signaledErr)
	require.True(t, turnSupervisorSignaledExit(signaledErr))
	require.True(t, turnSupervisorSignaledExit(fmt.Errorf("wrapped: %w", signaledErr)))
}

// TestParseTurnSupervisorLivenessReadyRefusesEveryMalformedReport proves the
// guardian only accepts a complete, positive native pid from its liveness
// helper. The parsed pid is handed to containment as a process group to kill,
// so a truncated line or a non-positive value would either address nothing or
// address the guardian's own group.
func TestParseTurnSupervisorLivenessReadyRefusesEveryMalformedReport(t *testing.T) {
	for _, testCase := range []struct {
		name string
		line string
		want string
	}{
		{name: "truncated", line: "ready:42", want: "not newline terminated"},
		{name: "wrong prefix", line: "armed\n", want: `invalid Claude liveness readiness "armed"`},
		{name: "non numeric pid", line: "ready:none\n", want: `invalid Claude liveness native pid "none"`},
		{name: "zero pid", line: "ready:0\n", want: `invalid Claude liveness native pid "0"`},
		{name: "negative pid", line: "ready:-1\n", want: `invalid Claude liveness native pid "-1"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pid, err := parseTurnSupervisorLivenessReady(testCase.line)
			require.ErrorContains(t, err, testCase.want)
			require.Zero(t, pid)
		})
	}

	pid, err := parseTurnSupervisorLivenessReady("ready:4242\n")
	require.NoError(t, err)
	require.Equal(t, 4242, pid)
}

// TestReadTurnSupervisorStartGateAcceptsOnlyTheReleaseToken proves the start
// gate is a token rather than any byte. The parent writes the token only after
// the supervisor is armed and it has taken its own containment hold, so a
// supervisor that launched on an arbitrary byte — or on a closed gate — would
// run the native root outside that hold.
func TestReadTurnSupervisorStartGateAcceptsOnlyTheReleaseToken(t *testing.T) {
	require.ErrorIs(t, readTurnSupervisorStartGate(strings.NewReader("")), io.EOF)
	require.ErrorContains(
		t, readTurnSupervisorStartGate(bytes.NewReader([]byte{0})), "invalid start gate token 0",
	)
	require.NoError(t, readTurnSupervisorStartGate(bytes.NewReader([]byte{1})))
}

// TestCompleteTurnSupervisorLivenessPublishesOnlyWhatItProved proves the
// liveness helper publishes nothing when it did not contain its tree, and
// reports rather than swallows a publication it could not make. A "done" line
// tells the guardian the turn ended cleanly and the proof byte tells the parent
// the tree is contained; either one published without containment would retire
// a live tree.
func TestCompleteTurnSupervisorLivenessPublishesOnlyWhatItProved(t *testing.T) {
	original := agentIdentityLockClose
	t.Cleanup(func() { agentIdentityLockClose = original })
	agentIdentityLockClose = func(file *os.File) error { return file.Close() }

	locks := func(t *testing.T) (*agentIdentityLock, *agentIdentityLock) {
		t.Helper()

		identity, err := os.CreateTemp(t.TempDir(), "identity")
		require.NoError(t, err)
		domain, err := os.CreateTemp(t.TempDir(), "domain")
		require.NoError(t, err)

		return &agentIdentityLock{file: identity}, &agentIdentityLock{file: domain}
	}

	t.Run("uncontained publishes nothing", func(t *testing.T) {
		identity, domain := locks(t)

		var ready, completion bytes.Buffer
		require.NoError(t, completeTurnSupervisorLiveness(
			nil, identity, domain, false, true, make(chan struct{}), &ready, &completion,
		))
		require.Zero(t, ready.Len())
		require.Zero(t, completion.Len())
	})

	t.Run("unpublishable proof is reported", func(t *testing.T) {
		identity, domain := locks(t)
		guardianDone := make(chan struct{})
		close(guardianDone)

		want := errors.New("proof")
		fault := &supervisorCovWriteFault{err: want}

		var ready bytes.Buffer
		err := completeTurnSupervisorLiveness(
			nil, identity, domain, true, false, guardianDone, &ready, fault,
		)
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "publish Claude liveness completion")
		require.Zero(t, ready.Len())
	})

	t.Run("unpublishable terminal result is reported", func(t *testing.T) {
		identity, domain := locks(t)

		want := errors.New("terminal")
		fault := &supervisorCovWriteFault{err: want}

		var completion bytes.Buffer
		err := completeTurnSupervisorLiveness(
			nil, identity, domain, true, false, make(chan struct{}), fault, &completion,
		)
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "publish Claude liveness terminal result")
		require.Zero(t, completion.Len())
	})
}

// TestAwaitProcessTreeReadyRefusesEveryPostArmedDeviation proves the parent
// keeps the start gate shut unless the supervisor reports readiness exactly.
// The gate is what holds the native launch until the parent has taken its
// containment hold, so a parent that released it on an unreadable or
// unrecognised second line would let the tree start unheld.
func TestAwaitProcessTreeReadyRefusesEveryPostArmedDeviation(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	t.Run("start gate cannot be released", func(t *testing.T) {
		read, write := supervisorCovPipe(t)
		_, err := io.WriteString(write, turnSupervisorArmed)
		require.NoError(t, err)

		_, gateWrite := supervisorCovPipe(t)
		require.NoError(t, gateWrite.Close())

		err = awaitProcessTreeReady(&processTreeCommand{ready: read, startGate: gateWrite})
		require.ErrorContains(t, err, "release Claude native supervisor start gate")
		require.ErrorIs(t, err, os.ErrClosed)
	})

	t.Run("readiness never arrives", func(t *testing.T) {
		read, write := supervisorCovPipe(t)
		_, err := io.WriteString(write, turnSupervisorArmed)
		require.NoError(t, err)
		require.NoError(t, write.Close())

		_, gateWrite := supervisorCovPipe(t)

		err = awaitProcessTreeReady(&processTreeCommand{ready: read, startGate: gateWrite})
		require.ErrorContains(t, err, "await Claude native supervisor readiness")
		require.ErrorIs(t, err, io.EOF)
	})

	t.Run("readiness is unrecognised", func(t *testing.T) {
		read, write := supervisorCovPipe(t)
		_, err := io.WriteString(write, turnSupervisorArmed+"started\n")
		require.NoError(t, err)

		_, gateWrite := supervisorCovPipe(t)

		err = awaitProcessTreeReady(&processTreeCommand{ready: read, startGate: gateWrite})
		require.ErrorContains(t, err, `invalid Claude native supervisor readiness "started"`)
	})
}

// TestStartTurnSupervisorNativeAbandonsTheLaunchOnAnyPrivilegeStep proves the
// native root is never started when a privilege step fails, and that the
// failure is reported as a privilege failure rather than a start failure. The
// caller distinguishes the two: a privilege failure means nothing was launched
// and nothing needs containing.
func TestStartTurnSupervisorNativeAbandonsTheLaunchOnAnyPrivilegeStep(t *testing.T) {
	restoreTurnSupervisorSeams(t)

	for _, testCase := range []struct {
		name  string
		fault func(error)
		want  string
	}{
		{
			name:  "core dumps",
			fault: func(err error) { turnSupervisorCoreLimit = func() error { return err } },
			want:  "disable core dumps for supervised Claude native root",
		},
		{
			name:  "no new privileges",
			fault: func(err error) { turnSupervisorNoNewPrivs = func() error { return err } },
			want:  "disable privilege elevation for supervised Claude native root",
		},
		{
			name:  "subreaper claim",
			fault: func(err error) { turnSupervisorEnable = func() error { return err } },
			want:  "subreaper",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			turnSupervisorCoreLimit = func() error { return nil }
			turnSupervisorNoNewPrivs = func() error { return nil }
			turnSupervisorEnable = func() error { return nil }

			want := errors.New(testCase.want)
			testCase.fault(want)

			native := exec.Command("/bin/true")
			waitDone, privilegeErr, startErr := startTurnSupervisorNative(
				native, supervisorCovIsolation(), nil,
			)
			require.Nil(t, waitDone)
			require.NoError(t, startErr)
			require.ErrorIs(t, privilegeErr, want)
			require.Nil(t, native.Process, "the native root was launched without its privilege bootstrap")
		})
	}

	t.Run("isolation the wrapper would refuse", func(t *testing.T) {
		turnSupervisorCoreLimit = func() error { return nil }
		turnSupervisorNoNewPrivs = func() error { return nil }
		turnSupervisorEnable = func() error { return nil }

		native := exec.Command("/bin/true")
		waitDone, privilegeErr, startErr := startTurnSupervisorNative(
			native, &ProcessIsolation{UID: 0, GID: 0}, nil,
		)
		require.Nil(t, waitDone)
		require.NoError(t, startErr)
		require.ErrorContains(t, privilegeErr, "apply Claude native process isolation")
		require.Nil(t, native.Process)
		require.Nil(t, native.SysProcAttr, "a refused isolation still reached the command")
	})
}

// TestStartTurnSupervisorNativeRunsTheRootAsTheTargetIdentity proves the
// launched native root really carries the dropped credential rather than the
// trusted identity that started it, and that the pre-start hook runs before the
// launch. The credential is applied on the creator thread immediately before
// the start, so a hook that ran afterwards could not refuse the launch.
func TestStartTurnSupervisorNativeRunsTheRootAsTheTargetIdentity(t *testing.T) {
	supervisorCovRequireRoot(t)
	restoreTurnSupervisorSeams(t)
	turnSupervisorCoreLimit = func() error { return nil }
	turnSupervisorNoNewPrivs = func() error { return nil }
	turnSupervisorEnable = func() error { return nil }

	isolation := supervisorCovIsolation()
	root := testTraversableTempDir(t)
	proof := filepath.Join(root, "identity")
	require.NoError(t, os.Chown(root, int(isolation.UID), int(isolation.GID)))

	native := exec.Command("/bin/sh", "-c", `id -u > "$1"; id -g >> "$1"`, "probe", proof)
	native.Dir = "/"
	native.Env = []string{"PATH=/usr/bin:/bin"}

	hooked := false
	waitDone, privilegeErr, startErr := startTurnSupervisorNative(native, isolation, func() error {
		hooked = native.Process == nil

		return nil
	})
	require.NoError(t, privilegeErr)
	require.NoError(t, startErr)
	require.NotNil(t, waitDone)
	require.NoError(t, <-waitDone)
	require.True(t, hooked, "the pre-start hook ran after the native root was launched")

	identity, err := os.ReadFile(proof)
	require.NoError(t, err)
	require.Equal(
		t,
		strconv.FormatUint(uint64(isolation.UID), 10)+"\n"+strconv.FormatUint(uint64(isolation.GID), 10),
		strings.TrimSpace(string(identity)),
	)
}

// TestPrepareProcessTreeCommandRefusesEveryUnusableAuthorityShape proves the
// parent refuses to build a supervisor launch it could not describe. The sealed
// config is the supervisor's only instruction, so isolation it would refuse, a
// non-root wrapper, and a half-supplied borrowed capability all have to fail
// before any descriptor is created.
func TestPrepareProcessTreeCommandRefusesEveryUnusableAuthorityShape(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	native := exec.Command("/bin/true")

	turnSupervisorEffectiveUID = func() int { return 0 }
	_, err := prepareProcessTreeCommand(native, processLaunchOptions{
		Isolation: &ProcessIsolation{UID: 0, GID: 0},
	})
	require.ErrorContains(t, err, "prepare Claude native supervisor isolation")

	turnSupervisorEffectiveUID = func() int { return 1000 }
	_, err = prepareProcessTreeCommand(native, processLaunchOptions{Isolation: supervisorCovIsolation()})
	require.ErrorContains(t, err, "prepare Claude native supervisor identity")

	// A half-supplied borrowed capability is refused by the isolation check
	// before the supervisor's own pairing guard can see it, so assert the
	// refusal that actually fires rather than the one that reads as the
	// supervisor's.
	turnSupervisorEffectiveUID = func() int { return 0 }
	borrowed := supervisorCovIsolation()
	borrowed.StandaloneOwnerID = ""
	borrowed.StandaloneStateRoot = ""
	borrowed.IdentityLock = &agentIdentityLock{}
	_, err = prepareProcessTreeCommand(native, processLaunchOptions{Isolation: borrowed})
	require.ErrorContains(t, err, "process identity lock and authority domain must be provided together")
}

// TestPrepareProcessTreeCommandRefusesUnduplicableBorrowedCapabilities proves a
// borrowed launch is abandoned when either capability cannot be duplicated for
// the supervisor. The supervisor receives its own descriptors so the parent can
// release its copies; handing it only one of the pair would leave it holding an
// identity with no matching authority domain, which the guardian would then
// adopt as complete.
func TestPrepareProcessTreeCommandRefusesUnduplicableBorrowedCapabilities(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorEffectiveUID = func() int { return 0 }

	lockFile, err := os.CreateTemp(t.TempDir(), "identity")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lockFile.Close() })

	native := exec.Command("/bin/true")

	borrowed := supervisorCovIsolation()
	borrowed.StandaloneOwnerID = ""
	borrowed.StandaloneStateRoot = ""
	borrowed.IdentityLock = &agentIdentityLock{}
	borrowed.AuthorityDomain = &agentIdentityLock{file: lockFile}
	_, err = prepareProcessTreeCommand(native, processLaunchOptions{Isolation: borrowed})
	require.ErrorContains(t, err, "duplicate Claude agent identity lock")

	borrowed.IdentityLock = &agentIdentityLock{file: lockFile}
	borrowed.AuthorityDomain = &agentIdentityLock{}
	_, err = prepareProcessTreeCommand(native, processLaunchOptions{Isolation: borrowed})
	require.ErrorContains(t, err, "duplicate Claude agent authority domain")

	borrowed.AuthorityDomain = &agentIdentityLock{file: lockFile}
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{Isolation: borrowed})
	require.NoError(t, err)
	t.Cleanup(launch.close)

	// A borrowed launch carries two more descriptors than a standalone one and
	// declares the origin that makes the guardian look for them.
	require.Len(t, launch.inherited, 7)

	var sealed turnSupervisorConfig
	require.NoError(t, json.NewDecoder(launch.inherited[0]).Decode(&sealed))
	require.Equal(t, turnSupervisorBorrowed, sealed.AuthorityOrigin)
	require.True(t, sealed.IdentityLock)
	require.True(t, sealed.AuthorityDomain)
}
