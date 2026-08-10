//go:build linux

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type supervisorTestSignal string

func (s supervisorTestSignal) String() string { return string(s) }
func (supervisorTestSignal) Signal()          {}

type supervisorWriteSeeker struct {
	writeErr error
	seekErr  error
}

const turnSupervisorSecurityLimitsProofEnv = "ACP_GO_CLAUDE_TEST_SECURITY_LIMITS_PROOF"

func TestTurnSupervisorNativeInheritsSecurityLimits(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted supervisor credential boundary requires root")
	}
	proofPath := os.Getenv(turnSupervisorSecurityLimitsProofEnv)
	if proofPath != "" {
		native := exec.Command("/bin/sh", "-c", `nnp=$(awk '$1 == "NoNewPrivs:" { print $2 }' /proc/self/status); printf '%s %s\n' "$nnp" "$(ulimit -c)" > "$1"`, "sh", proofPath)
		isolation := testProcessIsolation()
		isolation.identityAuthorityAdopted = true
		isolation.StandaloneOwnerID = ""
		isolation.StandaloneStateRoot = ""
		waitDone, enableErr, startErr := startTurnSupervisorNative(native, isolation, nil)
		if err := errors.Join(enableErr, startErr); err != nil {
			t.Fatal(err)
		}
		if err := <-waitDone; err != nil {
			t.Fatal(err)
		}

		return
	}

	proofRoot := testTraversableTempDir(t)
	require.NoError(t, os.Chown(proofRoot, 1, 1))
	proofPath = filepath.Join(proofRoot, "security-limits")
	helper := exec.Command(os.Args[0], "-test.run=^TestTurnSupervisorNativeInheritsSecurityLimits$")
	helper.Env = append(os.Environ(), turnSupervisorSecurityLimitsProofEnv+"="+proofPath)
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("run security-limits proof helper: %v\n%s", err, output)
	}

	proof, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(proof)) != "1 0" {
		t.Fatalf("native security limits = %q, want 1 0", strings.TrimSpace(string(proof)))
	}
}

func (w supervisorWriteSeeker) Write(value []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}

	return len(value), nil
}

func (w supervisorWriteSeeker) Seek(int64, int) (int64, error) {
	return 0, w.seekErr
}

func restoreTurnSupervisorSeams(t *testing.T) {
	t.Helper()
	executable := turnSupervisorExecutable
	memfd := turnSupervisorMemfd
	sealConfig := turnSupervisorSealConfig
	pipe := turnSupervisorPipe
	exit := turnSupervisorExit
	notify := turnSupervisorSignalNotify
	stop := turnSupervisorSignalStop
	input := turnSupervisorInput
	enable := turnSupervisorEnable
	noNewPrivs := turnSupervisorNoNewPrivs
	coreLimit := turnSupervisorCoreLimit
	command := turnSupervisorCommand
	contain := turnSupervisorContain
	processID := turnSupervisorProcessID
	signalGroup := turnSupervisorSignalGroup
	writeConfig := turnSupervisorWriteConfig
	descendants := turnSupervisorDescendants
	identity := turnSupervisorIdentity
	signalPID := turnSupervisorSignalPID
	wait4 := turnSupervisorWait4
	sleep := turnSupervisorSleep
	procRoot := turnSupervisorProcRoot
	beforeRelease := turnSupervisorBeforeRelease
	run := turnSupervisorRun
	openFile := turnSupervisorOpenFile
	fcntl := turnSupervisorFcntl
	acquireStandalone := turnSupervisorAcquireStandalone
	runLiveness := turnSupervisorRunLiveness
	prctl := turnSupervisorPrctl
	setrlimit := turnSupervisorSetrlimit
	poll := turnSupervisorPoll
	effectiveUID := turnSupervisorEffectiveUID
	syscallKillOriginal := syscallKill
	t.Cleanup(func() {
		turnSupervisorExecutable = executable
		turnSupervisorMemfd = memfd
		turnSupervisorSealConfig = sealConfig
		turnSupervisorPipe = pipe
		turnSupervisorExit = exit
		turnSupervisorSignalNotify = notify
		turnSupervisorSignalStop = stop
		turnSupervisorInput = input
		turnSupervisorEnable = enable
		turnSupervisorNoNewPrivs = noNewPrivs
		turnSupervisorCoreLimit = coreLimit
		turnSupervisorCommand = command
		turnSupervisorContain = contain
		turnSupervisorProcessID = processID
		turnSupervisorSignalGroup = signalGroup
		turnSupervisorWriteConfig = writeConfig
		turnSupervisorDescendants = descendants
		turnSupervisorIdentity = identity
		turnSupervisorSignalPID = signalPID
		turnSupervisorWait4 = wait4
		turnSupervisorSleep = sleep
		turnSupervisorProcRoot = procRoot
		turnSupervisorBeforeRelease = beforeRelease
		turnSupervisorRun = run
		turnSupervisorOpenFile = openFile
		turnSupervisorFcntl = fcntl
		turnSupervisorAcquireStandalone = acquireStandalone
		turnSupervisorRunLiveness = runLiveness
		turnSupervisorPrctl = prctl
		turnSupervisorSetrlimit = setrlimit
		turnSupervisorPoll = poll
		turnSupervisorEffectiveUID = effectiveUID
		syscallKill = syscallKillOriginal
	})
}

func TestTrustedSupervisorPreservesCapturedNativeOutput(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted supervisor credential boundary requires root")
	}
	const (
		uid = uint32(64201)
		gid = uint32(64202)
	)
	output, err := containedClaudeOutput(
		t.Context(),
		"/bin/sh",
		[]string{"-c", `printf '2.1.80 (Claude Code)\n'`},
		Options{
			Cwd: "/",
			ProcessIsolation: &ProcessIsolation{
				UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
				StandaloneOwnerID: "claude-captured-output", StandaloneStateRoot: createClaudeSupervisorStateRoot(t, uid, gid),
			},
		},
		nil,
		"claude version",
	)
	if err != nil {
		t.Fatalf("capture supervised output: %v", err)
	}
	if got := string(output); got != "2.1.80 (Claude Code)\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestTurnSupervisorBootstrapBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	t.Setenv(turnSupervisorModeEnv, turnSupervisorMode)

	exitCode := -1
	turnSupervisorExit = func(code int) { exitCode = code }
	failureOutput := &bytes.Buffer{}
	turnSupervisorInput = func() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
		return nil, nil, &recordingWriteCloser{Writer: failureOutput}, errors.New("input")
	}
	turnSupervisorBootstrap()
	if exitCode != 1 {
		t.Fatalf("input failure exit = %d, want 1", exitCode)
	}
	if !strings.Contains(failureOutput.String(), turnSupervisorFailure+"input") {
		t.Fatalf("input failure readiness = %q", failureOutput.String())
	}

	closed := make([]bool, 3)
	turnSupervisorInput = func() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
		return &recordingReadCloser{Reader: strings.NewReader("config"), closed: &closed[0]},
			&recordingReadCloser{Reader: strings.NewReader("control"), closed: &closed[1]},
			&recordingWriteCloser{Writer: io.Discard, closed: &closed[2]}, nil
	}
	turnSupervisorRun = func(io.Reader, io.Reader, io.Writer) error { return nil }
	turnSupervisorBootstrap()
	if exitCode != 0 || !closed[0] || !closed[1] || !closed[2] {
		t.Fatalf("successful bootstrap = exit %d, closed %v", exitCode, closed)
	}

	t.Setenv(turnSupervisorModeEnv, "")
	exitCode = -1
	turnSupervisorBootstrap()
	if exitCode != -1 {
		t.Fatalf("disabled bootstrap exited with %d", exitCode)
	}
}

func TestInheritedTurnSupervisorInputAndEnable(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	if err := enableTurnSupervisor(); err != nil {
		t.Fatalf("enable subreaper: %v", err)
	}
	// PR_SET_CHILD_SUBREAPER is process-global and inherited by later tests.
	// Restore the test binary immediately so shuffled execution cannot turn
	// unrelated process-group zombies into children that this test never reaps.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0); err != nil {
		t.Fatalf("disable subreaper: %v", err)
	}

	turnSupervisorOpenFile = func(uintptr, string) *os.File { return nil }
	if _, _, _, err := inheritedTurnSupervisorInput(); err == nil {
		t.Fatal("missing inherited descriptors succeeded")
	}

	files := make([]*os.File, 0, 3)
	writes := make([]*os.File, 0, 3)
	for range 3 {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, read)
		writes = append(writes, write)
	}
	t.Cleanup(func() {
		for _, file := range writes {
			_ = file.Close()
		}
	})
	next := 0
	turnSupervisorOpenFile = func(uintptr, string) *os.File {
		file := files[next]
		next++

		return file
	}
	closeOnExec := 0
	turnSupervisorFcntl = func(uintptr, int, int) (int, error) {
		closeOnExec++

		return 0, nil
	}
	config, control, ready, err := inheritedTurnSupervisorInput()
	if err != nil {
		t.Fatalf("inherited input: %v", err)
	}
	_ = config.Close()
	_ = control.Close()
	_ = ready.Close()
	if closeOnExec != 6 {
		t.Fatalf("close-on-exec calls = %d", closeOnExec)
	}
}

type recordingReadCloser struct {
	io.Reader
	closed *bool
}

func (c *recordingReadCloser) Close() error {
	*c.closed = true

	return nil
}

type recordingWriteCloser struct {
	io.Writer
	closed *bool
}

func (c *recordingWriteCloser) Close() error {
	if c.closed != nil {
		*c.closed = true
	}

	return nil
}

func TestPrepareTurnSupervisorBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorEffectiveUID = func() int { return 0 }
	options := processLaunchOptions{Isolation: testProcessIsolation()}

	if _, err := prepareProcessTreeCommand(&exec.Cmd{}, options); err == nil {
		t.Fatal("incomplete native command was accepted")
	}

	native := exec.Command("true")
	turnSupervisorMemfd = func(string, int) (int, error) { return 0, errors.New("memfd") }
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("memfd failure was ignored")
	}

	turnSupervisorMemfd = unix.MemfdCreate
	turnSupervisorWriteConfig = func(io.WriteSeeker, turnSupervisorConfig) error { return errors.New("write") }
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("config write failure was ignored")
	}
	turnSupervisorWriteConfig = writeTurnSupervisorConfig
	turnSupervisorSealConfig = func(uintptr, int, int) (int, error) { return 0, errors.New("seal") }
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("config seal failure was ignored")
	}
	turnSupervisorSealConfig = unix.FcntlInt

	pipeCalls := 0
	turnSupervisorPipe = func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 1 {
			return nil, nil, errors.New("control pipe")
		}

		return os.Pipe()
	}
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("control pipe failure was ignored")
	}

	pipeCalls = 0
	turnSupervisorPipe = func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 2 {
			return nil, nil, errors.New("ready pipe")
		}

		return os.Pipe()
	}
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("readiness pipe failure was ignored")
	}

	pipeCalls = 0
	turnSupervisorPipe = func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 3 {
			return nil, nil, errors.New("proof pipe")
		}

		return os.Pipe()
	}
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("proof pipe failure was ignored")
	}

	pipeCalls = 0
	turnSupervisorPipe = func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 4 {
			return nil, nil, errors.New("start gate pipe")
		}

		return os.Pipe()
	}
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("start gate pipe failure was ignored")
	}

	turnSupervisorPipe = os.Pipe
	turnSupervisorExecutable = func() (string, error) { return "", errors.New("executable") }
	if _, err := prepareProcessTreeCommand(native, options); err == nil {
		t.Fatal("executable failure was ignored")
	}

	turnSupervisorExecutable = os.Executable
	launch, err := prepareProcessTreeCommand(native, options)
	if err != nil {
		t.Fatalf("prepare supervisor: %v", err)
	}
	if launch.cmd == nil || len(launch.inherited) != 5 || launch.startGate == nil || launch.control == nil || launch.ready == nil || launch.proof == nil {
		t.Fatalf("prepared launch = %#v", launch)
	}
	if launch.cmd.Dir != "/" || len(launch.cmd.Env) != 1 || launch.cmd.Env[0] != turnSupervisorModeEnv+"="+turnSupervisorMode {
		t.Fatalf("supervisor authority environment = dir %q env %q", launch.cmd.Dir, launch.cmd.Env)
	}
	if launch.cmd.SysProcAttr == nil || launch.cmd.SysProcAttr.Credential != nil {
		t.Fatalf("supervisor unexpectedly carries native credentials: %#v", launch.cmd.SysProcAttr)
	}
	var sealed turnSupervisorConfig
	if err := json.NewDecoder(launch.inherited[0]).Decode(&sealed); err != nil {
		t.Fatalf("decode sealed supervisor config: %v", err)
	}
	if sealed.Isolation.UID != options.Isolation.UID || sealed.Isolation.GID != options.Isolation.GID {
		t.Fatalf("sealed native identity = %d:%d", sealed.Isolation.UID, sealed.Isolation.GID)
	}
	launch.close()
	launch.close()
}

func TestTurnSupervisorLivenessCarriesGuardianStdio(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorExecutable = func() (string, error) { return "/bin/true", nil }
	var launched *exec.Cmd
	turnSupervisorCommand = func(name string, args ...string) *exec.Cmd {
		launched = exec.Command(name, args...)

		return launched
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	completionRead, completionWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer completionRead.Close()
	defer completionWrite.Close()

	liveness, data, peer, start, err := startTurnSupervisorLiveness(
		turnSupervisorConfig{}, controlRead, completionWrite, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	defer peer.Close()
	defer start.Close()
	if launched == nil || liveness != launched {
		t.Fatalf("launched liveness command = %#v, want %#v", liveness, launched)
	}
	if launched.Stdin != os.Stdin || launched.Stdout != os.Stdout || launched.Stderr != os.Stderr {
		t.Fatalf("liveness stdio = (%T, %T, %T), want guardian stdio", launched.Stdin, launched.Stdout, launched.Stderr)
	}
	if err = liveness.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestTurnSupervisorConfigAndReadinessBranches(t *testing.T) {
	writeErr := errors.New("write")
	if err := writeTurnSupervisorConfig(supervisorWriteSeeker{writeErr: writeErr}, turnSupervisorConfig{}); !errors.Is(err, writeErr) {
		t.Fatalf("write config error = %v", err)
	}
	seekErr := errors.New("seek")
	if err := writeTurnSupervisorConfig(supervisorWriteSeeker{seekErr: seekErr}, turnSupervisorConfig{}); !errors.Is(err, seekErr) {
		t.Fatalf("seek config error = %v", err)
	}

	if err := awaitProcessTreeReady(&processTreeCommand{}); err != nil {
		t.Fatalf("nil readiness: %v", err)
	}
	regular, err := os.CreateTemp(t.TempDir(), "ready")
	if err != nil {
		t.Fatal(err)
	}
	if err := awaitProcessTreeReady(&processTreeCommand{ready: regular}); err == nil {
		t.Fatal("regular-file readiness deadline unexpectedly succeeded")
	}

	for _, test := range []struct {
		name  string
		value string
		ok    bool
	}{
		{name: "eof"},
		{name: "invalid", value: "bad\n"},
		{name: "failure", value: "armed\nerror:guardian failed\n"},
		{name: "ready", value: "armed\nready\n", ok: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			read, write, pipeErr := os.Pipe()
			if pipeErr != nil {
				t.Fatal(pipeErr)
			}
			if test.value != "" {
				_, _ = io.WriteString(write, test.value)
			}
			_ = write.Close()
			err := awaitProcessTreeReady(&processTreeCommand{ready: read})
			if test.ok && err != nil {
				t.Fatalf("readiness = %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("invalid readiness succeeded")
			}
		})
	}
}

// TestProcessTreeReadinessSpansTheStandaloneClaimBudget proves that the managed
// root's wait for the guardian's armed report is at least as long as the
// standalone identity claim the report waits behind, and that the guardian's
// own post-claim wait is deliberately held apart from it.
//
// The guardian writes turnSupervisorArmed only after
// acquireTurnSupervisorAuthority has returned, and that claim is allowed
// agentStandaloneClaimMax to finish. A readiness wait shorter than the claim
// budget therefore cancels a claim that was still making progress and reports a
// native supervisor readiness failure that never happened. The first case
// reports the armed state one second past the retired five-second budget and
// requires the wait to accept it. The second case pins the distinction: the
// guardian arms turnSupervisorLivenessReadyWait only after its own claim has
// already completed, so that budget does not span a claim and must not be
// raised to the claim maximum along with the readiness wait.
func TestProcessTreeReadinessSpansTheStandaloneClaimBudget(t *testing.T) {
	require.Less(t, turnSupervisorLivenessReadyWait, agentStandaloneClaimMax,
		"post-claim liveness wait is no longer held apart from the claim budget")

	report := turnSupervisorLivenessReadyWait + time.Second
	require.Less(t, report, agentStandaloneClaimMax,
		"readiness report does not fit inside the claim budget")

	read, write, err := os.Pipe()
	require.NoError(t, err)

	reported := make(chan struct{})
	t.Cleanup(func() {
		<-reported
		_ = write.Close()
	})
	go func() {
		defer close(reported)
		time.Sleep(report)
		_, _ = io.WriteString(write, turnSupervisorArmed+turnSupervisorReady)
	}()

	started := time.Now()
	require.NoError(t, awaitProcessTreeReady(&processTreeCommand{ready: read}),
		"armed state reported %v into the claim", report)
	require.GreaterOrEqual(t, time.Since(started), report,
		"the readiness wait did not outlast the retired budget")
}

func TestTurnSupervisorEnvironmentReplacesInternalMode(t *testing.T) {
	t.Setenv(turnSupervisorModeEnv, "stale")

	env := turnSupervisorEnvironment()

	count := 0
	for _, entry := range env {
		if entry == turnSupervisorModeEnv+"="+turnSupervisorMode {
			count++
		}
		if entry == turnSupervisorModeEnv+"=stale" {
			t.Fatal("stale supervisor mode survived")
		}
	}
	if count != 1 {
		t.Fatalf("supervisor mode count = %d", count)
	}

	// The trusted supervisor runs with a closed environment: nothing ambient,
	// and no identity stamp it could be talked out of by an inherited value.
	values := environmentMap(env)
	if len(values) != 1 {
		t.Fatalf("supervisor environment = %#v", values)
	}
}

func encodeSupervisorConfig(t *testing.T, config turnSupervisorConfig) io.Reader {
	t.Helper()
	if config.Isolation.UID == 0 {
		config.Isolation.UID = 1
	}
	if config.Isolation.GID == 0 {
		config.Isolation.GID = 1
	}
	if !config.IdentityLock && !config.AuthorityDomain {
		config.AuthorityOrigin = turnSupervisorStandalone
		config.Isolation.StandaloneOwnerID = "test-owner"
		config.Isolation.StandaloneStateRoot = "/var/lib/acp-go-test"
	} else if config.AuthorityOrigin == "" {
		config.AuthorityOrigin = turnSupervisorBorrowed
	}
	if config.Isolation.BaseEnvironment == nil {
		config.Isolation.BaseEnvironment = map[string]string{"PATH": "/usr/bin:/bin"}
	}
	if config.Env == nil {
		config.Env = environmentList(config.Isolation.BaseEnvironment)
	}
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(config); err != nil {
		t.Fatal(err)
	}

	return bytes.NewReader(buffer.Bytes())
}

func TestContainLinuxSupervisorDescendantsBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorSignalGroup = func(int, syscall.Signal) error { return errors.New("ignored") }
	waits := 0
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
		waits++
		if waits == 1 {
			return 0, nil
		}

		return -1, syscall.ECHILD
	}
	retryCalls := 0
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) {
		retryCalls++

		return nil, errors.New("retry")
	}
	turnSupervisorSleep = func(time.Duration) {}
	if err := awaitLinuxSupervisorContainment(1, 2); err != nil || retryCalls != 1 {
		t.Fatalf("await containment = %v after %d calls", err, retryCalls)
	}

	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 0, nil }
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) { return nil, errors.New("list") }
	if err := containLinuxSupervisorDescendants(1, 2); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("list failure = %v", err)
	}

	waits = 0
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
		waits++
		if waits == 1 {
			return 0, nil
		}

		return -1, syscall.ECHILD
	}
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) { return nil, nil }
	if err := containLinuxSupervisorDescendants(1, 2); err != nil {
		t.Fatalf("empty tree: %v", err)
	}

	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
		return -1, errors.New("wait")
	}
	if err := containLinuxSupervisorDescendants(1, 2); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("wait failure = %v", err)
	}

	descendant := linuxProcessIdentity{pid: 3, state: 'S', startTime: "1"}
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 0, nil }
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) { return []linuxProcessIdentity{descendant}, nil }
	turnSupervisorSignalPID = func(linuxProcessIdentity, syscall.Signal) error { return errors.New("kill") }
	if err := containLinuxSupervisorDescendants(1, 2); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("kill failure = %v", err)
	}

	calls := 0
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) {
		calls++
		if calls == 1 {
			return []linuxProcessIdentity{{pid: 2, state: 'Z'}, descendant}, nil
		}

		return nil, nil
	}
	signals := 0
	turnSupervisorSignalPID = func(linuxProcessIdentity, syscall.Signal) error {
		signals++

		return nil
	}
	waits = 0
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
		waits++
		if waits == 1 {
			return 3, nil
		}
		if waits == 2 {
			return 0, syscall.EINTR
		}
		if waits == 3 {
			return 0, nil
		}

		return -1, syscall.ECHILD
	}
	turnSupervisorSleep = func(time.Duration) {}
	if err := containLinuxSupervisorDescendants(1, 2); err != nil {
		t.Fatalf("contain descendants: %v", err)
	}
	if signals != 1 || waits != 4 {
		t.Fatalf("containment signals=%d waits=%d", signals, waits)
	}
}

func TestLinuxProcessInventoryAndIdentityBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	root := t.TempDir()
	turnSupervisorProcRoot = root

	if _, err := readLinuxProcessIdentity(1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing identity = %v", err)
	}
	writeProcStat(t, root, 1, "malformed")
	if _, err := readLinuxProcessIdentity(1); err == nil {
		t.Fatal("malformed comm succeeded")
	}
	writeProcStat(t, root, 1, "1 (cmd) S")
	if _, err := readLinuxProcessIdentity(1); err == nil {
		t.Fatal("incomplete stat succeeded")
	}
	writeProcStat(t, root, 1, procStatLine(1, "bad", "10"))
	if _, err := readLinuxProcessIdentity(1); err == nil {
		t.Fatal("bad parent succeeded")
	}

	writeProcStat(t, root, 1, procStatLine(1, "0", "10"))
	writeProcStat(t, root, 2, procStatLine(2, "1", "20"))
	writeProcStat(t, root, 3, procStatLine(3, "2", "30"))
	if err := os.Mkdir(filepath.Join(root, "not-a-pid"), 0o700); err != nil {
		t.Fatal(err)
	}
	descendants, err := linuxDescendants(1)
	if err != nil || len(descendants) != 2 || descendants[0].pid != 2 || descendants[1].pid != 3 {
		t.Fatalf("descendants = %#v, %v", descendants, err)
	}

	if err := os.Mkdir(filepath.Join(root, "4"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxDescendants(1); err != nil {
		t.Fatalf("vanished process should be skipped: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "5"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "5", "stat"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxDescendants(1); err == nil {
		t.Fatal("unreadable process stat was ignored")
	}

	turnSupervisorProcRoot = filepath.Join(root, "missing")
	if _, err := linuxDescendants(1); err == nil {
		t.Fatal("missing proc root succeeded")
	}
}

func writeProcStat(t *testing.T, root string, pid int, value string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func procStatLine(pid int, parent string, start string) string {
	fields := []string{"S", parent}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, start)

	return strconv.Itoa(pid) + " (command with spaces) " + strings.Join(fields, " ")
}

func TestSignalLinuxIdentityBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	want := linuxProcessIdentity{pid: 7, startTime: "10"}

	turnSupervisorIdentity = func(int) (linuxProcessIdentity, error) { return linuxProcessIdentity{}, os.ErrNotExist }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); err != nil {
		t.Fatalf("missing identity: %v", err)
	}
	turnSupervisorIdentity = func(int) (linuxProcessIdentity, error) { return linuxProcessIdentity{startTime: "11"}, nil }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); err != nil {
		t.Fatalf("reused identity: %v", err)
	}
	wantErr := errors.New("identity")
	turnSupervisorIdentity = func(int) (linuxProcessIdentity, error) { return linuxProcessIdentity{}, wantErr }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); !errors.Is(err, wantErr) {
		t.Fatalf("identity error = %v", err)
	}

	turnSupervisorIdentity = func(int) (linuxProcessIdentity, error) { return want, nil }
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); err != nil {
		t.Fatalf("ESRCH signal: %v", err)
	}
	syscallKill = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signal error = %v", err)
	}
	syscallKill = func(int, syscall.Signal) error { return nil }
	if err := signalLinuxIdentity(want, syscall.SIGKILL); err != nil {
		t.Fatalf("signal identity: %v", err)
	}
}

func TestProcessIsolationActualClaudeTrustedSupervisorIdentityGroupsAmbientAndContainment(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted supervisor credential boundary requires root")
	}
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}

	const (
		uid = uint32(64081)
		gid = uint32(64082)
	)
	root := createClaudeSupervisorFixtureRoot(t, uid, gid)
	status := filepath.Join(root, "native", "status")
	daemon := filepath.Join(root, "native", "daemon.pid")
	script := `supervisor=$PPID
if kill -STOP "$supervisor" 2>/dev/null; then echo stop=allowed; else echo stop=blocked; fi > "$1"
if printf 'forged\n' > "/proc/$supervisor/fd/3" 2>/dev/null; then echo forge=allowed; else echo forge=blocked; fi >> "$1"
groups=$(sed -n 's/^Groups:[[:space:]]*//p' "/proc/$$/status")
if [ -z "$groups" ]; then echo groups=empty; else echo groups="$groups"; fi >> "$1"
echo uid=$(id -u) >> "$1"
echo gid=$(id -g) >> "$1"
if [ "${ACP_GO_CLAUDE_TEST_ACTUAL_AMBIENT+x}" = x ]; then echo ambient=leaked; else echo ambient=scrubbed; fi >> "$1"
authorityfds=none
for fd in /proc/self/fd/*; do
  target=$(readlink "$fd" 2>/dev/null || true)
  case "$target" in /var/lib/acp-go/agent-identities/*) authorityfds=leaked;; esac
done
echo authorityfds=$authorityfds >> "$1"
"$3" sh -c 'trap "" INT TERM HUP; while :; do sleep 30; done' & echo $! > "$2"
if kill -KILL "$supervisor" 2>/dev/null; then echo kill=allowed; else echo kill=blocked; fi >> "$1"`
	native := exec.Command("/bin/sh", "-c", script, "probe", status, daemon, setsid)
	native.Dir = "/"
	native.Env = []string{"PATH=/usr/bin:/bin"}
	isolation := &ProcessIsolation{
		UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID: "claude-trusted-supervisor", StandaloneStateRoot: createClaudeSupervisorStateRoot(t, uid, gid),
	}
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{Isolation: isolation})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startContainedProcess(launch)
	if err != nil {
		t.Fatalf("start trusted Claude supervisor: %v", err)
	}
	waiter := startChildExit(tree, launch.cmd)
	t.Cleanup(func() {
		_ = tree.close()
		for _, path := range []string{daemon} {
			if pid, readErr := readClaudeSupervisorPIDFile(path); readErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = syscall.Kill(tree.processGroupID, syscall.SIGCONT)
		_ = syscall.Kill(-tree.processGroupID, syscall.SIGKILL)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = waiter.await(ctx)
		cancel()
	})

	want := "stop=blocked\nforge=blocked\ngroups=empty\nuid=64081\ngid=64082\nambient=scrubbed\nauthorityfds=none\nkill=blocked\n"
	awaitClaudeSupervisorFile(t, status, want)
	if err = tree.complete(10 * time.Second); err != nil {
		t.Fatalf("quiesce trusted Claude supervisor: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	waitErr, completed := waiter.await(ctx)
	cancel()
	if !completed {
		t.Fatalf("await trusted Claude supervisor: %v", waitErr)
	}
	if errors.Is(waitErr, ErrProcessContainmentIncomplete) {
		t.Fatalf("trusted Claude supervisor lost containment proof: %v", waitErr)
	}
	daemonPID, err := readClaudeSupervisorPIDFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	awaitClaudeSupervisorProcessGone(t, daemonPID)
	assertClaudeSupervisorAuthorityLocks(t, "/var/lib/acp-go/agent-identities", uid, true)
}

func TestTurnSupervisorNativeGuardianPeerDeathBeforeStartUnitBranches(t *testing.T) {
	restoreTurnSupervisorSeams(t)
	turnSupervisorSignalNotify = func(chan<- os.Signal, ...os.Signal) {}
	turnSupervisorSignalStop = func(chan<- os.Signal) {}
	turnSupervisorEffectiveUID = func() int { return 0 }

	peerRead, peerWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	guardian := exec.Command("/bin/sleep", "30")
	guardian.ExtraFiles = []*os.File{peerWrite}
	if err = guardian.Start(); err != nil {
		t.Fatal(err)
	}
	_ = peerWrite.Close()
	guardianWaited := false
	t.Cleanup(func() {
		_ = peerRead.Close()
		if !guardianWaited {
			_ = guardian.Process.Kill()
			_ = guardian.Wait()
		}
	})
	turnSupervisorCoreLimit = func() error {
		if err = guardian.Process.Kill(); err != nil {
			return err
		}
		waitErr := guardian.Wait()
		guardianWaited = true
		if waitErr == nil {
			return errors.New("SIGKILLed guardian exited successfully")
		}

		return nil
	}
	turnSupervisorNoNewPrivs = func() error { return nil }
	turnSupervisorEnable = func() error { return nil }

	identityFile, err := os.CreateTemp(t.TempDir(), "identity-lock")
	if err != nil {
		t.Fatal(err)
	}
	domainFile, err := os.CreateTemp(t.TempDir(), "domain-lock")
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(identityFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(domainFile.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	turnSupervisorAcquireStandalone = func(uint32, uint32, string, string, bool, string, <-chan struct{}, <-chan os.Signal) (*agentStandaloneIdentity, error) {
		return &agentStandaloneIdentity{
			identity: &agentIdentityLock{file: identityFile}, authority: &agentIdentityLock{file: domainFile},
		}, nil
	}

	echild := false
	turnSupervisorWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) {
		echild = true

		return -1, unix.ECHILD
	}
	turnSupervisorDescendants = func(int) ([]linuxProcessIdentity, error) { return nil, nil }
	turnSupervisorContain = awaitLinuxSupervisorContainment
	marker := filepath.Join(t.TempDir(), "native-launched")
	var native *exec.Cmd
	turnSupervisorCommand = func(name string, args ...string) *exec.Cmd {
		native = exec.Command(name, args...)

		return native
	}
	config := encodeSupervisorConfig(t, turnSupervisorConfig{
		Path: "/bin/sh", Args: []string{"sh", "-c", "touch \"$1\"", "probe", marker},
	})
	var ready, completion bytes.Buffer
	err = runTurnSupervisorNative(
		config, []io.Reader{strings.NewReader("control")}, peerRead, strings.NewReader("\x01"),
		&ready, &completion, 6, 7,
	)
	if err == nil || !strings.Contains(err.Error(), "guardian exited before native launch") {
		t.Fatalf("pre-readiness guardian death = %v", err)
	}
	if native == nil || native.Process != nil {
		t.Fatalf("native command started after guardian death: %#v", native)
	}
	if !echild || ready.String() != turnSupervisorArmed || completion.String() != turnSupervisorProof {
		t.Fatalf("pre-readiness proof ECHILD=%t ready=%q completion=%q", echild, ready.String(), completion.String())
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native launch marker exists after guardian death: %v", err)
	}
}

func TestTurnSupervisorCompletionClosesAuthorityBeforeProof(t *testing.T) {
	originalClose := agentIdentityLockClose
	t.Cleanup(func() { agentIdentityLockClose = originalClose })

	newAuthority := func(t *testing.T) (*turnSupervisorAuthority, []*os.File) {
		t.Helper()
		identity, err := os.CreateTemp(t.TempDir(), "identity")
		if err != nil {
			t.Fatal(err)
		}
		domain, err := os.CreateTemp(t.TempDir(), "domain")
		if err != nil {
			t.Fatal(err)
		}

		return &turnSupervisorAuthority{
			identity: &agentIdentityLock{file: identity},
			domain:   &agentIdentityLock{file: domain},
		}, []*os.File{identity, domain}
	}

	t.Run("guardian success", func(t *testing.T) {
		authority, _ := newAuthority(t)
		var order []string
		agentIdentityLockClose = func(file *os.File) error {
			order = append(order, "close")

			return file.Close()
		}
		completion := &bytes.Buffer{}
		completionWriter := io.MultiWriter(completion, supervisorOrderWriter{order: &order})
		if err := completeTurnSupervisorAuthority(completionWriter, &authority, true); err != nil {
			t.Fatal(err)
		}
		if authority != nil || completion.String() != turnSupervisorProof || !slices.Equal(order, []string{"close", "close", "proof"}) {
			t.Fatalf("guardian completion authority=%v proof=%q order=%v", authority, completion.String(), order)
		}
	})

	t.Run("guardian close failure", func(t *testing.T) {
		authority, files := newAuthority(t)
		want := errors.New("close identity")
		calls := 0
		agentIdentityLockClose = func(file *os.File) error {
			calls++
			if calls == 1 {
				return want
			}

			return file.Close()
		}
		var completion bytes.Buffer
		if err := completeTurnSupervisorAuthority(&completion, &authority, true); !errors.Is(err, want) {
			t.Fatalf("guardian close error = %v", err)
		}
		if authority != nil || calls != 2 || completion.Len() != 0 {
			t.Fatalf("guardian failure authority=%v calls=%d proof=%q", authority, calls, completion.String())
		}
		_ = files[0].Close()
	})

	t.Run("liveness close failure after guardian death", func(t *testing.T) {
		authority, files := newAuthority(t)
		guardianDone := make(chan struct{})
		close(guardianDone)
		want := errors.New("close identity")
		calls := 0
		agentIdentityLockClose = func(file *os.File) error {
			calls++
			if calls == 1 {
				return want
			}

			return file.Close()
		}
		var ready, completion bytes.Buffer
		err := completeTurnSupervisorLiveness(
			nil, authority.identity, authority.domain, true, false, guardianDone, &ready, &completion,
		)
		if !errors.Is(err, want) || calls != 2 || completion.Len() != 0 || !strings.HasPrefix(ready.String(), turnSupervisorFailure) {
			t.Fatalf("liveness failure error=%v calls=%d ready=%q proof=%q", err, calls, ready.String(), completion.String())
		}
		_ = files[0].Close()
	})

	t.Run("liveness routes completion by guardian state", func(t *testing.T) {
		for _, guardianExited := range []bool{false, true} {
			t.Run(strconv.FormatBool(guardianExited), func(t *testing.T) {
				authority, _ := newAuthority(t)
				agentIdentityLockClose = func(file *os.File) error { return file.Close() }
				guardianDone := make(chan struct{})
				if guardianExited {
					close(guardianDone)
				}
				var ready, completion bytes.Buffer
				if err := completeTurnSupervisorLiveness(
					nil, authority.identity, authority.domain, true, guardianExited, guardianDone, &ready, &completion,
				); err != nil {
					t.Fatal(err)
				}
				if guardianExited {
					if ready.Len() != 0 || completion.String() != turnSupervisorProof {
						t.Fatalf("survivor ready=%q completion=%q", ready.String(), completion.String())
					}
				} else if ready.String() != "done\n" || completion.Len() != 0 {
					t.Fatalf("paired ready=%q completion=%q", ready.String(), completion.String())
				}
			})
		}
	})
}

type supervisorOrderWriter struct {
	order *[]string
}

func (writer supervisorOrderWriter) Write(value []byte) (int, error) {
	*writer.order = append(*writer.order, "proof")

	return len(value), nil
}

type claudeSupervisorPeerDeathFixture struct {
	tree           *processContainment
	waiter         *commandWait
	guardianPID    int
	livenessPID    int
	descendantPIDs []int
	uid            uint32
}

func TestProcessIsolationSupervisorGuardianSIGKILLBeforeNativeLaunchRefusesStartAndCompletesAfterECHILD(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted supervisor credential boundary requires root")
	}
	if err := validateAgentStandaloneBinder(); err != nil {
		t.Skipf("trusted supervisor binder is unavailable: %v", err)
	}
	restoreTurnSupervisorSeams(t)
	const (
		uid = uint32(64211)
		gid = uint32(64212)
	)
	root := createClaudeSupervisorFixtureRoot(t, uid, gid)
	marker := filepath.Join(root, "native", "launched")
	native := exec.Command("/bin/sh", "-c", `touch "$1"`, "probe", marker)
	stateRoot := createClaudeSupervisorStateRoot(t, uid, gid)
	isolation := &ProcessIsolation{
		UID:                 uid,
		GID:                 gid,
		BaseEnvironment:     map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID:   "claude-before-native-guardian-death",
		StandaloneStateRoot: stateRoot,
	}
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{Isolation: isolation})
	if err != nil {
		t.Fatal(err)
	}
	killed := errors.New("guardian killed before native release")
	turnSupervisorBeforeRelease = func(process *os.Process) error {
		if process == nil {
			return errors.New("guardian process is unavailable")
		}
		if killErr := process.Kill(); killErr != nil {
			return killErr
		}
		awaitClaudeSupervisorProcessDead(t, process.Pid)

		return killed
	}
	tree, err := startContainedProcess(launch)
	if tree != nil || !errors.Is(err, killed) {
		t.Fatalf("pre-native guardian death tree=%#v err=%v", tree, err)
	}
	if errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("pre-native guardian death lost ECHILD containment proof: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("native launch marker exists after pre-native guardian death: %v", statErr)
	}
	assertClaudeSupervisorAuthorityLocks(t, "/var/lib/acp-go/agent-identities", uid, true)
}

func TestProcessIsolationSupervisorGuardianSIGKILLRetainsAuthorityThroughECHILD(t *testing.T) {
	fixture := startClaudeSupervisorPeerDeathFixture(t, 64131, 64132, "claude-guardian-death")
	exerciseClaudeSupervisorPeerDeath(t, fixture, fixture.livenessPID, fixture.guardianPID)
}

func TestProcessIsolationSupervisorLivenessSIGKILLRetainsAuthorityThroughECHILD(t *testing.T) {
	fixture := startClaudeSupervisorPeerDeathFixture(t, 64141, 64142, "claude-liveness-death")
	exerciseClaudeSupervisorPeerDeath(t, fixture, fixture.guardianPID, fixture.livenessPID)
}

func startClaudeSupervisorPeerDeathFixture(t *testing.T, uid, gid uint32, ownerID string) *claudeSupervisorPeerDeathFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("dual trusted supervisor containment requires root")
	}
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}

	root := createClaudeSupervisorFixtureRoot(t, uid, gid)
	state := filepath.Join(root, "native")
	leaf := filepath.Join(root, "leaf.sh")
	double := filepath.Join(root, "double.sh")
	nativeScript := filepath.Join(root, "native.sh")
	if err = os.WriteFile(leaf, []byte("#!/bin/sh\ntrap '' INT TERM HUP\necho $$ > \"$1\"\nwhile :; do sleep 30; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(double, []byte("#!/bin/sh\n\"$1\" \"$2\" &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nativeBody := `#!/bin/sh
set -eu
echo $$ > "$CLAUDE_TEST_NATIVE_PID"
"$CLAUDE_TEST_LEAF" "$CLAUDE_TEST_ORDINARY_PID" &
"$CLAUDE_TEST_SETSID" "$CLAUDE_TEST_LEAF" "$CLAUDE_TEST_SESSION_PID" &
"$CLAUDE_TEST_SETSID" "$CLAUDE_TEST_DOUBLE" "$CLAUDE_TEST_LEAF" "$CLAUDE_TEST_DOUBLE_PID" &
while [ ! -s "$CLAUDE_TEST_ORDINARY_PID" ] || [ ! -s "$CLAUDE_TEST_SESSION_PID" ] || [ ! -s "$CLAUDE_TEST_DOUBLE_PID" ]; do
  sleep 0.01
done
while :; do sleep 30; done
`
	if err = os.WriteFile(nativeScript, []byte(nativeBody), 0o755); err != nil {
		t.Fatal(err)
	}

	pidPaths := []string{
		filepath.Join(state, "root.pid"),
		filepath.Join(state, "ordinary.pid"),
		filepath.Join(state, "session.pid"),
		filepath.Join(state, "double.pid"),
	}
	native := exec.Command(nativeScript)
	native.Dir = "/"
	native.Env = []string{
		"PATH=/usr/bin:/bin",
		"CLAUDE_TEST_NATIVE_PID=" + pidPaths[0],
		"CLAUDE_TEST_ORDINARY_PID=" + pidPaths[1],
		"CLAUDE_TEST_SESSION_PID=" + pidPaths[2],
		"CLAUDE_TEST_DOUBLE_PID=" + pidPaths[3],
		"CLAUDE_TEST_LEAF=" + leaf,
		"CLAUDE_TEST_DOUBLE=" + double,
		"CLAUDE_TEST_SETSID=" + setsid,
	}
	isolation := &ProcessIsolation{
		UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID: ownerID, StandaloneStateRoot: createClaudeSupervisorStateRoot(t, uid, gid),
	}
	launch, err := prepareProcessTreeCommand(native, processLaunchOptions{Isolation: isolation})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startContainedProcess(launch)
	if err != nil {
		t.Fatalf("start dual trusted Claude supervisor fixture: %v", err)
	}
	waiter := startChildExit(tree, launch.cmd)
	fixture := &claudeSupervisorPeerDeathFixture{
		tree: tree, waiter: waiter, guardianPID: tree.processGroupID, uid: uid,
	}
	for _, path := range pidPaths {
		fixture.descendantPIDs = append(fixture.descendantPIDs, awaitClaudeSupervisorPIDFile(t, path))
	}
	identity, err := readLinuxProcessIdentity(fixture.descendantPIDs[0])
	if err != nil {
		t.Fatalf("read Claude native parent identity: %v", err)
	}
	fixture.livenessPID = identity.parentPID
	if fixture.livenessPID <= 0 || fixture.livenessPID == fixture.guardianPID {
		t.Fatalf("invalid Claude guardian/liveness topology guardian=%d liveness=%d", fixture.guardianPID, fixture.livenessPID)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(fixture.guardianPID, syscall.SIGCONT)
		_ = syscall.Kill(fixture.livenessPID, syscall.SIGCONT)
		_ = fixture.tree.close()
		pids := append([]int(nil), fixture.descendantPIDs...)
		pids = append(pids, fixture.guardianPID, fixture.livenessPID)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = fixture.waiter.await(ctx)
		cancel()
	})

	return fixture
}

func exerciseClaudeSupervisorPeerDeath(t *testing.T, fixture *claudeSupervisorPeerDeathFixture, survivorPID, victimPID int) {
	t.Helper()
	if err := syscall.Kill(survivorPID, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop surviving Claude supervisor %d: %v", survivorPID, err)
	}
	awaitClaudeSupervisorProcessState(t, survivorPID, 'T')
	if err := syscall.Kill(victimPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill Claude supervisor peer %d: %v", victimPID, err)
	}
	awaitClaudeSupervisorProcessDead(t, victimPID)
	assertClaudeSupervisorAuthorityLocks(t, "/var/lib/acp-go/agent-identities", fixture.uid, false)
	for _, pid := range fixture.descendantPIDs {
		if !processExists(pid) {
			t.Fatalf("Claude supervised descendant %d exited before survivor containment", pid)
		}
	}

	if err := syscall.Kill(survivorPID, syscall.SIGCONT); err != nil {
		t.Fatalf("resume surviving Claude supervisor %d: %v", survivorPID, err)
	}
	if err := fixture.tree.complete(10 * time.Second); err != nil {
		t.Fatalf("quiesce Claude supervisor after peer death: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	waitErr, completed := fixture.waiter.await(ctx)
	cancel()
	if !completed {
		t.Fatalf("await dual trusted Claude supervisor completion: %v", waitErr)
	}
	if errors.Is(waitErr, ErrProcessContainmentIncomplete) {
		t.Fatalf("dual trusted Claude supervisor lost containment proof: %v", waitErr)
	}
	for _, pid := range fixture.descendantPIDs {
		awaitClaudeSupervisorProcessGone(t, pid)
	}
	assertClaudeSupervisorAuthorityLocks(t, "/var/lib/acp-go/agent-identities", fixture.uid, true)
}

// createClaudeSupervisorFixtureRoot builds the native fixture tree under /tmp
// because the supervised native identity must be able to search every ancestor
// of the fixture. A checkout cannot serve: it sits under the invoking user's
// home, and Ubuntu creates home directories mode 0750.
func createClaudeSupervisorFixtureRoot(t *testing.T, uid, gid uint32) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "acp-go-claude-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "native")
	if err = os.Mkdir(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(native, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	return root
}

func createClaudeSupervisorStateRoot(t *testing.T, uid, gid uint32) string {
	t.Helper()
	base := "/var/lib/acp-go-claude-test"
	if err := os.Mkdir(base, 0o711); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o711 || stat.Uid != 0 || stat.Gid != 0 {
		t.Fatalf("Claude supervisor state parent has unsafe metadata: mode=%v stat=%#v", info.Mode(), stat)
	}
	state := filepath.Join(base, strconv.FormatUint(uint64(uid), 10))
	if err = os.Mkdir(state, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if err = os.Chown(state, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}

	return state
}

func awaitClaudeSupervisorFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(path)
		if err == nil && string(value) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	value, err := os.ReadFile(path)
	t.Fatalf("Claude supervisor file %q = %q, %v; want %q", path, value, err, want)
}

func awaitClaudeSupervisorPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readClaudeSupervisorPIDFile(path); err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude supervised process pid file %q was not published", path)

	return 0
}

func readClaudeSupervisorPIDFile(path string) (int, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid file %q", path)
	}

	return pid, nil
}

func awaitClaudeSupervisorProcessState(t *testing.T, pid int, want byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		identity, err := readLinuxProcessIdentity(pid)
		if err == nil && identity.state == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude supervised process %d did not enter state %q", pid, want)
}

func awaitClaudeSupervisorProcessDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		identity, err := readLinuxProcessIdentity(pid)
		if errors.Is(err, os.ErrNotExist) || err == nil && identity.state == 'Z' {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude supervisor peer %d did not exit after SIGKILL", pid)
}

func awaitClaudeSupervisorProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("Claude supervised descendant %d survived ECHILD containment proof", pid)
	}
}

func assertClaudeSupervisorAuthorityLocks(t *testing.T, authorityRoot string, uid uint32, available bool) {
	t.Helper()
	for _, name := range []string{strconv.FormatUint(uint64(uid), 10) + ".lock", "domain.lock"} {
		path := filepath.Join(authorityRoot, name)
		deadline := time.Now().Add(5 * time.Second)
		for {
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open Claude authority contender %q: %v", name, err)
			}
			lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if lockErr == nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			}
			_ = file.Close()
			if !available {
				if !errors.Is(lockErr, unix.EWOULDBLOCK) && !errors.Is(lockErr, unix.EAGAIN) {
					t.Fatalf("Claude authority lock %q was not retained by frozen survivor: %v", name, lockErr)
				}

				break
			}
			if lockErr == nil {
				break
			}
			if !errors.Is(lockErr, unix.EWOULDBLOCK) && !errors.Is(lockErr, unix.EAGAIN) {
				t.Fatalf("reacquire Claude authority lock %q: %v", name, lockErr)
			}
			if time.Now().After(deadline) {
				t.Fatalf("Claude authority lock %q remained held after ECHILD", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
