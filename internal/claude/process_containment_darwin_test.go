//go:build darwin

package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type darwinCloseErrorReadCloser struct{ err error }

func (r darwinCloseErrorReadCloser) Read([]byte) (int, error) { return 0, os.ErrClosed }
func (r darwinCloseErrorReadCloser) Close() error             { return r.err }

func completedCommandWait(err error) *commandWait {
	waiter := &commandWait{done: make(chan struct{}), err: err}
	close(waiter.done)

	return waiter
}

func TestDarwinCleanupLadderMemoizationAndAbsenceTerminality(t *testing.T) {
	originalKill := syscallKill
	originalNow := darwinContainmentNow
	originalSleep := darwinContainmentSleep
	t.Cleanup(func() {
		syscallKill = originalKill
		darwinContainmentNow = originalNow
		darwinContainmentSleep = originalSleep
	})

	now := time.Now()
	darwinContainmentNow = func() time.Time { return now }
	darwinContainmentSleep = func(duration time.Duration) { now = now.Add(duration) }
	var signals []syscall.Signal
	probes := 0
	syscallKill = func(pid int, signal syscall.Signal) error {
		if pid != -42 {
			t.Fatalf("pid = %d", pid)
		}
		signals = append(signals, signal)
		if signal == 0 {
			probes++
			if probes >= 52 {
				return syscall.ESRCH
			}
		}

		return nil
	}
	finished := 0
	tree := &processContainment{
		processGroupID: 42,
		waiter:         completedCommandWait(nil),
		generation: &DarwinGeneration{RecordFinished: func(complete bool) error {
			if !complete {
				t.Fatal("successful cleanup recorded incomplete")
			}
			finished++

			return nil
		}},
	}
	if err := tree.quiesce(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if err := tree.quiesce(time.Hour); err != nil || finished != 1 {
		t.Fatalf("memoized cleanup error=%v finished=%d", err, finished)
	}
	firstKill := -1
	for index, signal := range signals {
		if signal == syscall.SIGKILL {
			firstKill = index

			break
		}
	}
	if len(signals) == 0 || signals[0] != syscall.SIGTERM || firstKill < 0 || signals[len(signals)-1] != 0 {
		t.Fatalf("signal ladder = %v", signals)
	}

	signals = nil
	absent := &processContainment{processGroupID: 43, waiter: completedCommandWait(nil), generation: &DarwinGeneration{}}
	syscallKill = func(_ int, signal syscall.Signal) error {
		signals = append(signals, signal)

		return syscall.ESRCH
	}
	if err := absent.quiesce(defaultCloseWait); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(signals, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals after first ESRCH = %v", signals)
	}
}

func TestDarwinCleanupFailureBranches(t *testing.T) {
	originalKill := syscallKill
	originalNow := darwinContainmentNow
	originalSleep := darwinContainmentSleep
	t.Cleanup(func() {
		syscallKill = originalKill
		darwinContainmentNow = originalNow
		darwinContainmentSleep = originalSleep
	})

	newTree := func() *processContainment {
		return &processContainment{processGroupID: 44, waiter: completedCommandWait(nil), generation: &DarwinGeneration{}}
	}

	want := errors.New("syscall")
	syscallKill = func(int, syscall.Signal) error { return want }
	if err := newTree().quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "terminate") {
		t.Fatalf("term error = %v", err)
	}

	calls := 0
	syscallKill = func(_ int, signal syscall.Signal) error {
		calls++
		if calls == 2 && signal == 0 {
			return want
		}

		return nil
	}
	if err := newTree().quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("probe error = %v", err)
	}

	now := time.Now()
	darwinContainmentNow = func() time.Time { return now }
	darwinContainmentSleep = func(duration time.Duration) { now = now.Add(duration) }
	syscallKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			return want
		}

		return nil
	}
	if err := newTree().quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "kill") {
		t.Fatalf("kill error = %v", err)
	}

	now = time.Now()
	killCalls := 0
	syscallKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			killCalls++

			return syscall.ESRCH
		}

		return nil
	}
	if err := newTree().quiesce(defaultCloseWait); err != nil || killCalls != 1 {
		t.Fatalf("kill ESRCH cleanup error=%v calls=%d", err, killCalls)
	}

	syscallKill = func(int, syscall.Signal) error { return nil }
	if err := newTree().quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "remained observable") {
		t.Fatalf("deadline error = %v", err)
	}

	if err := (*processContainment)(nil).quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("nil cleanup error = %v", err)
	}
	if err := (&processContainment{}).quiesce(defaultCloseWait); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("missing identity cleanup error = %v", err)
	}
}

func TestDarwinWaitNormalizesPipeDelayOnlyAfterContainment(t *testing.T) {
	originalKill := syscallKill
	t.Cleanup(func() { syscallKill = originalKill })
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	tree := &processContainment{
		processGroupID: 45,
		waiter:         completedCommandWait(exec.ErrWaitDelay),
		generation:     &DarwinGeneration{},
	}
	if err := tree.wait(nil); err != nil {
		t.Fatalf("wait error = %v", err)
	}
	if !tree.ownsShutdown() || tree.close() != nil {
		t.Fatal("Darwin tree ownership mismatch")
	}
	if count, exact := tree.processSnapshot(); count != 0 || exact {
		t.Fatalf("best-effort snapshot = (%d,%v)", count, exact)
	}

	unreaped := &processContainment{waiter: &commandWait{done: make(chan struct{})}, generation: &DarwinGeneration{}}
	unreaped.cleanupOnce.Do(func() { unreaped.cleanupErr = ErrProcessContainmentIncomplete })
	if err := unreaped.wait(nil); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "not reaped") {
		t.Fatalf("unreaped wait error = %v", err)
	}
}

func prepareDarwinTestLaunch(t *testing.T, generation *DarwinGeneration) *processTreeCommand {
	t.Helper()
	if generation == nil {
		generation = &DarwinGeneration{ScratchRoot: t.TempDir()}
	}
	launch, err := prepareProcessTreeCommand(exec.Command("/bin/sh", "-c", "while :; do sleep 1; done"), processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       generation,
	})
	if err != nil {
		t.Fatal(err)
	}

	return launch
}

func TestDarwinStartValidationAndFastExitBranches(t *testing.T) {
	originalGetpgid := syscallGetpgid
	originalKill := syscallKill
	originalAbortWait := darwinAbortWait
	originalAbortKill := darwinAbortKillAfter
	originalFastWait := darwinFastExitWait
	t.Cleanup(func() {
		syscallGetpgid = originalGetpgid
		syscallKill = originalKill
		darwinAbortWait = originalAbortWait
		darwinAbortKillAfter = originalAbortKill
		darwinFastExitWait = originalFastWait
	})
	darwinAbortWait = 50 * time.Millisecond
	darwinAbortKillAfter = 5 * time.Millisecond
	darwinFastExitWait = 100 * time.Millisecond

	for _, test := range []struct {
		name string
		get  func(int) (int, error)
	}{
		{name: "probe-error", get: func(int) (int, error) { return 0, syscall.EIO }},
		{name: "mismatched-leader", get: func(pid int) (int, error) { return pid + 1, nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			syscallGetpgid = test.get
			tree, err := startContainedProcess(prepareDarwinTestLaunch(t, nil))
			if tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
				t.Fatalf("tree=%v error=%v", tree, err)
			}
		})
	}

	syscallGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if tree, err := startContainedProcess(prepareDarwinTestLaunch(t, nil)); tree != nil || err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("absent fast-exit tree=%v error=%v", tree, err)
	}

	probeCalls := 0
	syscallKill = func(_ int, signal syscall.Signal) error {
		probeCalls++
		if probeCalls == 1 && signal == 0 {
			return nil
		}

		return syscall.ESRCH
	}
	if tree, err := startContainedProcess(prepareDarwinTestLaunch(t, nil)); tree != nil || err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("observable fast-exit tree=%v error=%v", tree, err)
	}

	syscallKill = func(int, syscall.Signal) error { return syscall.EIO }
	if tree, err := startContainedProcess(prepareDarwinTestLaunch(t, nil)); tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("probe-failure fast-exit tree=%v error=%v", tree, err)
	}

	openWaiter := &commandWait{done: make(chan struct{})}
	manual := &processContainment{processGroupID: 99, waiter: openWaiter, generation: &DarwinGeneration{}}
	darwinFastExitWait = time.Millisecond
	syscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := handleDarwinFastExit(&processTreeCommand{}, manual, func() {}); !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "reap fast-exit") {
		t.Fatalf("fast-exit waiter timeout = %v", err)
	}
}

func TestDarwinGateReleaseFailureCleansGroup(t *testing.T) {
	originalKill := syscallKill
	originalGetpgid := syscallGetpgid
	originalWait := startPausedCommandWaitFn
	t.Cleanup(func() {
		syscallKill = originalKill
		syscallGetpgid = originalGetpgid
		startPausedCommandWaitFn = originalWait
	})
	launch := prepareDarwinTestLaunch(t, nil)
	if err := launch.startGate.Close(); err != nil {
		t.Fatal(err)
	}
	syscallGetpgid = func(pid int) (int, error) { return pid, nil }
	var order []string
	startPausedCommandWaitFn = func(wait func() error) (*commandWait, func()) {
		waiter, begin := startPausedCommandWait(wait)

		return waiter, func() {
			order = append(order, "waiter")
			begin()
		}
	}
	syscallKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGTERM {
			order = append(order, "signal")
		}

		return syscall.ESRCH
	}
	tree, err := startContainedProcess(launch)
	if tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "release validated") {
		t.Fatalf("tree=%v error=%v", tree, err)
	}
	if !reflect.DeepEqual(order, []string{"signal", "waiter"}) {
		t.Fatalf("gate-failure cleanup order = %v", order)
	}
}

func TestDarwinAbortUnvalidatedAndFailCleanupBranches(t *testing.T) {
	originalSignal := darwinDirectSignal
	originalKill := darwinDirectKill
	originalAbortWait := darwinAbortWait
	originalAbortKill := darwinAbortKillAfter
	t.Cleanup(func() {
		darwinDirectSignal = originalSignal
		darwinDirectKill = originalKill
		darwinAbortWait = originalAbortWait
		darwinAbortKillAfter = originalAbortKill
	})
	darwinAbortWait = 20 * time.Millisecond
	darwinAbortKillAfter = time.Millisecond
	want := errors.New("direct")

	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waiter, begin := startPausedCommandWait(command.Wait)
	begin()
	darwinDirectSignal = func(*os.Process, os.Signal) error { return want }
	tree := &processContainment{process: command.Process, waiter: waiter, generation: &DarwinGeneration{}}
	if err := tree.abortUnvalidated(); !errors.Is(err, ErrProcessContainmentIncomplete) || !errors.Is(err, want) {
		t.Fatalf("abort error = %v", err)
	}

	command = exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waiter, begin = startPausedCommandWait(command.Wait)
	begin()
	darwinDirectKill = func(*os.Process) error { return want }
	tree = &processContainment{process: command.Process, waiter: waiter, generation: &DarwinGeneration{}}
	err := tree.abortUnvalidated()
	if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "not reaped") {
		t.Fatalf("failed abort kill error = %v", err)
	}
	darwinDirectKill = originalKill
	_ = command.Process.Kill()
	_, _ = waiter.await(t.Context())

	darwinDirectKill = func(*os.Process) error { return os.ErrProcessDone }
	tree = &processContainment{process: &os.Process{Pid: 1}, waiter: completedCommandWait(nil), generation: &DarwinGeneration{}}
	if err := tree.failCleanup(time.Now().Add(time.Second), want); !errors.Is(err, ErrProcessContainmentIncomplete) || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("done cleanup error = %v", err)
	}
	darwinDirectKill = func(*os.Process) error { return want }
	if err := tree.failCleanup(time.Now().Add(time.Second), errors.New("cause")); !errors.Is(err, want) {
		t.Fatalf("failed direct cleanup error = %v", err)
	}
	tree = &processContainment{generation: &DarwinGeneration{}}
	if err := tree.failCleanup(time.Now().Add(time.Second), want); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("nil process/waiter cleanup = %v", err)
	}
	tree = &processContainment{waiter: &commandWait{done: make(chan struct{})}, generation: &DarwinGeneration{}}
	if err := tree.failCleanup(time.Now().Add(-time.Millisecond), want); !strings.Contains(err.Error(), "not reaped") {
		t.Fatalf("unreaped cleanup error = %v", err)
	}
}

func TestProcessTransportDarwinContainmentShutdownBranches(t *testing.T) {
	want := errors.New("containment")
	failedTree := &processContainment{processGroupID: 1, generation: &DarwinGeneration{}}
	failedTree.cleanupOnce.Do(func() { failedTree.cleanupErr = want })
	var inventories []func() (int, bool)
	transport := &ProcessTransport{
		tree: failedTree,
		options: Options{ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
			inventories = append(inventories, inventory)
		}},
	}
	require.ErrorIs(t, transport.quiesceProcessTree(), ErrProcessContainmentIncomplete)
	require.Len(t, inventories, 1)

	originalClose := processContainmentClose
	t.Cleanup(func() { processContainmentClose = originalClose })
	closedTree := &processContainment{processGroupID: 1, generation: &DarwinGeneration{}}
	closedTree.cleanupOnce.Do(func() {})
	quiesced := 0
	transport = &ProcessTransport{
		tree: closedTree,
		options: Options{ObserveProcessQuiesced: func(context.Context) {
			quiesced++
		}},
	}
	processContainmentClose = func(*processContainment) error { return want }
	require.ErrorIs(t, transport.quiesceProcessTree(), want)
	require.Equal(t, 1, quiesced)
	processContainmentClose = originalClose

	originalDelay := processShutdownWaitDelay
	t.Cleanup(func() { processShutdownWaitDelay = originalDelay })
	processShutdownWaitDelay = 0
	waitErr := make(chan error, 1)
	waitErr <- context.Canceled
	require.NoError(t, (&ProcessTransport{}).waitForShutdown(waitErr, true))

	processShutdownWaitDelay = time.Millisecond
	timerTree := &processContainment{processGroupID: 1, generation: &DarwinGeneration{}}
	timerTree.cleanupOnce.Do(func() {})
	transport = &ProcessTransport{tree: timerTree}
	require.ErrorContains(t, transport.waitForShutdown(make(chan error), false), processWaitTimedOutMessage)

	ownedTree := &processContainment{processGroupID: 1, generation: &DarwinGeneration{}}
	ownedTree.cleanupOnce.Do(func() {})
	transport = &ProcessTransport{tree: ownedTree}
	require.NoError(t, transport.shutdownProcess(false))

	waitErr = make(chan error, 1)
	waitErr <- context.Canceled
	require.ErrorIs(t, waitForProcessExit(waitErr, true, want, time.Second), want)
	transport = &ProcessTransport{stdout: darwinCloseErrorReadCloser{err: want}}
	require.ErrorIs(t, transport.closeStdout(), want)
}

func TestDarwinStartContainedProcessRealBoundaries(t *testing.T) {
	if tree, err := startContainedProcess(nil); tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("nil launch tree=%v error=%v", tree, err)
	}
	if tree, err := startContainedProcess(&processTreeCommand{}); tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("empty launch tree=%v error=%v", tree, err)
	}
	if tree, err := startContainedProcess(&processTreeCommand{cmd: exec.Command("true"), bestEffort: true, generation: &DarwinGeneration{}}); tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("ungrouped launch tree=%v error=%v", tree, err)
	}

	startFailed := &DarwinGeneration{}
	missing := &processTreeCommand{
		cmd:        exec.Command("/definitely/missing/acp-go-claude"),
		bestEffort: true,
		generation: startFailed,
	}
	configureProcessCommandPlatform(missing.cmd)
	if tree, err := startContainedProcess(missing); tree != nil || err == nil {
		t.Fatalf("missing launch tree=%v error=%v", tree, err)
	}

	generation := &DarwinGeneration{ScratchRoot: t.TempDir()}
	launch, err := prepareProcessTreeCommand(exec.Command("/bin/sh", "-c", "while :; do sleep 1; done"), processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startContainedProcess(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.quiesce(defaultCloseWait); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinSetsidEscapeSurvivesSelectedBoundary(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	readyFile := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=TestDarwinContainmentSetsidHelper")
	command.Env = append(os.Environ(),
		"CLAUDE_TEST_DARWIN_CONTAINMENT_ROLE=parent",
		"CLAUDE_TEST_DARWIN_CONTAINMENT_PID_FILE="+pidFile,
		"CLAUDE_TEST_DARWIN_CONTAINMENT_READY_FILE="+readyFile,
	)
	generation := &DarwinGeneration{RuntimeID: strings.Repeat("0", 32), ScratchRoot: t.TempDir()}
	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startContainedProcess(launch)
	if err != nil {
		t.Fatal(err)
	}

	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, statErr := os.Stat(readyFile); statErr == nil {
			ready = true

			break
		}
	}
	if !ready {
		t.Fatal("setsid escape helper did not become ready")
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	escapedPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if errors.Is(syscall.Kill(escapedPID, 0), syscall.ESRCH) {
				return
			}
		}
		t.Errorf("setsid escapee pid %d remained after test cleanup deadline", escapedPID)
	})

	if err := tree.quiesce(defaultCloseWait); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Fatalf("setsid escapee did not survive selected-boundary cleanup: %v", err)
	}
}

func TestDarwinContainmentSetsidHelper(t *testing.T) {
	switch os.Getenv("CLAUDE_TEST_DARWIN_CONTAINMENT_ROLE") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=TestDarwinContainmentSetsidHelper")
		child.Env = append(os.Environ(),
			"CLAUDE_TEST_DARWIN_CONTAINMENT_ROLE=child",
			"CLAUDE_TEST_DARWIN_CONTAINMENT_READY_FILE="+os.Getenv("CLAUDE_TEST_DARWIN_CONTAINMENT_READY_FILE"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("CLAUDE_TEST_DARWIN_CONTAINMENT_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(30 * time.Second)
	case "child":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("CLAUDE_TEST_DARWIN_CONTAINMENT_READY_FILE"), []byte("ready"), 0o600); err != nil {
			os.Exit(5)
		}
		time.Sleep(30 * time.Second)
	}
}

func TestDarwinRecordActivationFailureCleansCapturedGroup(t *testing.T) {
	want := errors.New("record")
	generation := &DarwinGeneration{
		ScratchRoot:   t.TempDir(),
		RecordStarted: func(int, int) error { return want },
	}
	launch, err := prepareProcessTreeCommand(exec.Command("/bin/sh", "-c", "while :; do sleep 1; done"), processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startContainedProcess(launch)
	if tree != nil || !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("activation tree=%v error=%v", tree, err)
	}
}
