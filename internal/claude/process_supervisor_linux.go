//go:build linux

package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	turnSupervisorModeEnv      = "ACP_GO_CLAUDE_INTERNAL_PROCESS_SUPERVISOR"
	turnSupervisorMode         = "guardian"
	turnSupervisorLivenessMode = "liveness"
	turnSupervisorFDName       = "acp-go-claude-native-supervisor"
	turnSupervisorReady        = "ready\n"
	turnSupervisorArmed        = "armed\n"
	turnSupervisorProof        = "contained\n"
	turnSupervisorDonePrefix   = "done:"
	turnSupervisorFailure      = "error:"
	turnSupervisorBorrowed     = "borrowed"
	turnSupervisorStandalone   = "standalone"
	turnSupervisorChallengeLen = 32
	turnSupervisorTerminalLen  = len(turnSupervisorDonePrefix) + 2*turnSupervisorChallengeLen + 1

	// turnSupervisorLivenessReadyWait bounds the guardian's wait for its
	// liveness child's readiness report. The guardian arms it only after
	// acquireTurnSupervisorAuthority has returned, so unlike the parent's
	// readiness wait it does not span the standalone identity claim and is
	// deliberately not tied to agentStandaloneClaimMax.
	turnSupervisorLivenessReadyWait = 5 * time.Second
)

type turnSupervisorConfig struct {
	Path            string           `json:"path"`
	Args            []string         `json:"args"`
	Dir             string           `json:"dir"`
	Env             []string         `json:"env"`
	Isolation       ProcessIsolation `json:"isolation"`
	IdentityLock    bool             `json:"identityLock"`
	AuthorityDomain bool             `json:"authorityDomain"`
	AuthorityOrigin string           `json:"authorityOrigin"`
}

type linuxProcessIdentity struct {
	pid       int
	parentPID int
	groupID   int
	state     byte
	startTime string
}

var (
	errTurnSupervisorGuardianExited = errors.New("claude guardian exited before native launch")

	turnSupervisorExecutable                  = os.Executable
	turnSupervisorMemfd                       = unix.MemfdCreate
	turnSupervisorPipe                        = os.Pipe
	turnSupervisorExit                        = os.Exit
	turnSupervisorSignalNotify                = signal.Notify
	turnSupervisorSignalStop                  = signal.Stop
	turnSupervisorEnable                      = enableTurnSupervisor
	turnSupervisorNoNewPrivs                  = enableTurnSupervisorNoNewPrivileges
	turnSupervisorCoreLimit                   = disableTurnSupervisorCoreDumps
	turnSupervisorCommand                     = exec.Command
	turnSupervisorContain                     = awaitLinuxSupervisorContainment
	turnSupervisorProcessID                   = os.Getpid
	turnSupervisorSignalGroup                 = signalProcessGroupID
	turnSupervisorWriteConfig                 = writeTurnSupervisorConfig
	turnSupervisorDescendants                 = linuxDescendants
	turnSupervisorIdentity                    = readLinuxProcessIdentity
	turnSupervisorSignalPID                   = signalLinuxIdentity
	turnSupervisorWait4                       = unix.Wait4
	turnSupervisorSleep                       = time.Sleep
	turnSupervisorProcRoot                    = "/proc"
	turnSupervisorRun                         = runTurnSupervisorGuardian
	turnSupervisorRunLiveness                 = runTurnSupervisorLiveness
	turnSupervisorOpenFile                    = os.NewFile
	turnSupervisorFcntl                       = unix.FcntlInt
	turnSupervisorInput                       = inheritedTurnSupervisorInput
	turnSupervisorPrctl                       = unix.Prctl
	turnSupervisorSetrlimit                   = unix.Setrlimit
	turnSupervisorAcquireStandalone           = acquireAgentStandaloneIdentity
	turnSupervisorSealConfig                  = unix.FcntlInt
	turnSupervisorEffectiveUID                = os.Geteuid
	turnSupervisorPoll                        = unix.Poll
	turnSupervisorReadDeadline                = (*os.File).SetReadDeadline
	turnSupervisorWritePeer                   = (*os.File).Write
	turnSupervisorChallengeSource   io.Reader = rand.Reader
	turnSupervisorBeforeRelease     func(*os.Process) error
	turnSupervisorAfterTerminalRead func()
	turnSupervisorBeforeTerminalACK func()
	turnSupervisorAfterTerminalACK  func()
)

func enableTurnSupervisor() error {
	if err := turnSupervisorSetrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fmt.Errorf("disable Claude native core dumps: %w", err)
	}

	if err := turnSupervisorPrctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}

	if err := turnSupervisorPrctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}

	return turnSupervisorPrctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

func enableTurnSupervisorNoNewPrivileges() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

func disableTurnSupervisorCoreDumps() error {
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})
}

func inheritedTurnSupervisorInput() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
	config := turnSupervisorOpenFile(3, "claude-turn-supervisor-config")
	control := turnSupervisorOpenFile(4, "claude-turn-supervisor-control")

	ready := turnSupervisorOpenFile(5, "claude-turn-supervisor-ready")
	if config == nil || control == nil || ready == nil {
		return nil, nil, nil, errors.New("native supervisor inherited descriptors are unavailable")
	}

	for _, file := range []*os.File{config, control, ready} {
		if err := setTurnSupervisorCloseOnExec(file); err != nil {
			_ = config.Close()
			_ = control.Close()
			_ = ready.Close()

			return nil, nil, nil, err
		}
	}

	return config, control, ready, nil
}

func setTurnSupervisorCloseOnExec(file *os.File) error {
	flags, err := turnSupervisorFcntl(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("read inherited Claude supervisor descriptor flags: %w", err)
	}

	if _, err = turnSupervisorFcntl(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("protect inherited Claude supervisor descriptor from exec: %w", err)
	}

	return nil
}

func init() {
	turnSupervisorBootstrap()
}

func turnSupervisorBootstrap() {
	mode := os.Getenv(turnSupervisorModeEnv)
	if mode != turnSupervisorMode && mode != turnSupervisorLivenessMode {
		return
	}

	var (
		err             error
		config, control io.ReadCloser
		ready           io.WriteCloser
	)

	config, control, ready, err = turnSupervisorInput()
	if err == nil {
		if mode == turnSupervisorLivenessMode {
			err = turnSupervisorRunLiveness(config, control, ready)
		} else {
			err = turnSupervisorRun(config, control, ready)
		}
	}

	if config != nil {
		_ = config.Close()
	}

	if control != nil {
		_ = control.Close()
	}

	if err != nil && ready != nil {
		_, _ = fmt.Fprintln(ready, turnSupervisorFailure+err.Error())
	}

	if ready != nil {
		_ = ready.Close()
	}

	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "acp-go-claude native supervisor:", err)

		turnSupervisorExit(1)

		return
	}

	turnSupervisorExit(0)
}

func prepareProcessTreeCommand(native *exec.Cmd, options processLaunchOptions) (*processTreeCommand, error) {
	// Omission is not a policy: it selects the ordinary directly-owned boundary,
	// which starts no trusted-root guardian and claims no identity authority.
	if options.Isolation == nil {
		return prepareOrdinaryLaunch(native, options)
	}

	if err := validateProcessIsolation(options.Isolation); err != nil {
		return nil, fmt.Errorf("prepare Claude native supervisor isolation: %w", err)
	}

	if err := validateTurnSupervisorIdentity(options.Isolation); err != nil {
		return nil, fmt.Errorf("prepare Claude native supervisor identity: %w", err)
	}

	config := turnSupervisorConfig{
		Path:            native.Path,
		Args:            append([]string(nil), native.Args...),
		Dir:             native.Dir,
		Env:             append([]string(nil), native.Env...),
		Isolation:       *options.Isolation,
		IdentityLock:    options.Isolation.IdentityLock != nil,
		AuthorityDomain: options.Isolation.AuthorityDomain != nil,
		AuthorityOrigin: turnSupervisorStandalone,
	}

	// The origin travels in the sealed config so the guardian and the liveness
	// child inherit the one decision the parent made. Each of them re-derives it
	// from its own state and refuses a config that disagrees, so the stamp can
	// direct the launch without being trusted on its own.
	if config.IdentityLock {
		config.AuthorityOrigin = turnSupervisorBorrowed
	}

	if config.Path == "" || len(config.Args) == 0 {
		return nil, errors.New("prepare Claude native supervisor: native command is incomplete")
	}

	configFD, err := turnSupervisorMemfd(turnSupervisorFDName, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("prepare Claude native supervisor config: %w", err)
	}

	configFile := os.NewFile(uintptr(configFD), turnSupervisorFDName)
	if writeErr := turnSupervisorWriteConfig(configFile, config); writeErr != nil {
		_ = configFile.Close()

		return nil, writeErr
	}

	if _, sealErr := turnSupervisorSealConfig(configFile.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); sealErr != nil {
		_ = configFile.Close()

		return nil, fmt.Errorf("seal Claude native supervisor config: %w", sealErr)
	}

	controlRead, controlWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()

		return nil, fmt.Errorf("prepare Claude native supervisor control: %w", err)
	}

	readyRead, readyWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()

		return nil, fmt.Errorf("prepare Claude native supervisor readiness: %w", err)
	}

	completionRead, completionWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()
		_ = readyRead.Close()
		_ = readyWrite.Close()

		return nil, fmt.Errorf("prepare Claude native supervisor completion: %w", err)
	}

	executable, err := turnSupervisorExecutable()
	if err != nil {
		_ = configFile.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = completionRead.Close()
		_ = completionWrite.Close()

		return nil, fmt.Errorf("resolve embedded Claude native supervisor: %w", err)
	}

	helper := turnSupervisorCommand(executable) // #nosec G204 -- the current executable hosts the private supervisor mode.
	helper.Dir = "/"
	helper.Env = turnSupervisorEnvironment()
	helper.Stdin = native.Stdin
	helper.Stdout = native.Stdout
	helper.Stderr = native.Stderr
	helper.ExtraFiles = []*os.File{configFile, controlRead, readyWrite, completionWrite}

	if options.Isolation.IdentityLock != nil {
		identityLock, duplicateErr := options.Isolation.IdentityLock.Duplicate()
		if duplicateErr != nil {
			_ = configFile.Close()
			_ = controlRead.Close()
			_ = controlWrite.Close()
			_ = readyRead.Close()
			_ = readyWrite.Close()
			_ = completionRead.Close()
			_ = completionWrite.Close()

			return nil, fmt.Errorf("duplicate Claude agent identity lock: %w", duplicateErr)
		}

		helper.ExtraFiles = append(helper.ExtraFiles, identityLock)

		authorityDomain, duplicateErr := options.Isolation.AuthorityDomain.Duplicate()
		if duplicateErr != nil {
			_ = identityLock.Close()
			_ = configFile.Close()
			_ = controlRead.Close()
			_ = controlWrite.Close()
			_ = readyRead.Close()
			_ = readyWrite.Close()
			_ = completionRead.Close()
			_ = completionWrite.Close()

			return nil, fmt.Errorf("duplicate Claude agent authority domain: %w", duplicateErr)
		}

		helper.ExtraFiles = append(helper.ExtraFiles, authorityDomain)
	}

	startRead, startWrite, err := turnSupervisorPipe()
	if err != nil {
		for _, file := range helper.ExtraFiles {
			_ = file.Close()
		}

		_ = controlWrite.Close()
		_ = readyRead.Close()
		_ = completionRead.Close()

		return nil, fmt.Errorf("prepare Claude native supervisor start gate: %w", err)
	}

	helper.ExtraFiles = append(helper.ExtraFiles, startRead)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	launch := &processTreeCommand{
		cmd:       helper,
		inherited: append([]*os.File(nil), helper.ExtraFiles...),
		startGate: startWrite,
		control:   controlWrite,
		ready:     readyRead,
		proof:     completionRead,
	}

	return launch, nil
}

func awaitProcessTreeReady(launch *processTreeCommand) error {
	if launch.ready == nil {
		return nil
	}

	defer func() {
		_ = launch.ready.Close()
		launch.ready = nil
	}()

	// The guardian reports its armed state only after it has acquired its
	// standalone agent identity, so this wait spans the claim and must never be
	// shorter than the claim's own maximum. Naming the claim budget here keeps
	// the two tied: a shorter wait would cancel a claim that was still
	// progressing and report a containment failure that never happened.
	if err := launch.ready.SetReadDeadline(time.Now().Add(agentStandaloneClaimMax)); err != nil {
		return fmt.Errorf("arm Claude native supervisor readiness: %w", err)
	}

	reader := bufio.NewReader(launch.ready)

	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("await Claude native supervisor readiness: %w", err)
	}

	if failure, ok := strings.CutPrefix(strings.TrimSpace(line), turnSupervisorFailure); ok {
		return fmt.Errorf("claude native supervisor failed before readiness: %s", failure)
	}

	if line != turnSupervisorArmed {
		return fmt.Errorf("invalid Claude native supervisor armed state %q", strings.TrimSpace(line))
	}

	if turnSupervisorBeforeRelease != nil {
		if err = turnSupervisorBeforeRelease(launch.cmd.Process); err != nil {
			launch.abortStartGate()

			return err
		}
	}

	if err = launch.releaseStartGate(); err != nil {
		return fmt.Errorf("release Claude native supervisor start gate: %w", err)
	}

	line, err = reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("await Claude native supervisor readiness: %w", err)
	}

	if failure, ok := strings.CutPrefix(strings.TrimSpace(line), turnSupervisorFailure); ok {
		return fmt.Errorf("claude native supervisor failed before readiness: %s", failure)
	}

	if line != turnSupervisorReady {
		return fmt.Errorf("invalid Claude native supervisor readiness %q", strings.TrimSpace(line))
	}

	return nil
}

func writeTurnSupervisorConfig(file io.WriteSeeker, config turnSupervisorConfig) error {
	if err := json.NewEncoder(file).Encode(config); err != nil {
		return fmt.Errorf("encode Claude native supervisor config: %w", err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind Claude native supervisor config: %w", err)
	}

	return nil
}

func turnSupervisorEnvironment() []string {
	return turnSupervisorEnvironmentFor(turnSupervisorMode)
}

func turnSupervisorEnvironmentFor(mode string) []string {
	return []string{
		turnSupervisorModeEnv + "=" + mode,
	}
}

func startTurnSupervisorNative(native *exec.Cmd, isolation *ProcessIsolation, beforeStart func() error) (<-chan error, error, error) {
	var privilegeErr error

	waitDone, startErr := startCommandOnCreatorThread(func() error {
		if err := turnSupervisorCoreLimit(); err != nil {
			privilegeErr = fmt.Errorf("disable core dumps for supervised Claude native root: %w", err)

			return privilegeErr
		}

		if err := turnSupervisorNoNewPrivs(); err != nil {
			privilegeErr = fmt.Errorf("disable privilege elevation for supervised Claude native root: %w", err)

			return privilegeErr
		}

		if err := turnSupervisorEnable(); err != nil {
			privilegeErr = err

			return err
		}

		if err := applyProcessCredential(native, isolation); err != nil {
			privilegeErr = fmt.Errorf("apply Claude native process isolation: %w", err)

			return privilegeErr
		}

		if beforeStart != nil {
			if err := beforeStart(); err != nil {
				return err
			}
		}

		return native.Start()
	}, native.Wait)

	if privilegeErr != nil {
		return nil, privilegeErr, nil
	}

	return waitDone, nil, startErr
}

func runTurnSupervisorGuardian(configInput io.Reader, controlInput io.Reader, readyOutput io.Writer) (runErr error) {
	completion := turnSupervisorOpenFile(6, "claude-turn-supervisor-completion")
	if completion == nil {
		return errors.New("claude guardian completion descriptor is unavailable")
	}
	defer completion.Close()

	if err := setTurnSupervisorCloseOnExec(completion); err != nil {
		return err
	}

	controlFile, ok := controlInput.(*os.File)
	if !ok {
		_, _ = completion.WriteString(turnSupervisorProof)

		return errors.New("claude guardian control input is not an inheritable file")
	}

	var config turnSupervisorConfig
	if err := json.NewDecoder(configInput).Decode(&config); err != nil {
		_, _ = completion.WriteString(turnSupervisorProof)

		return fmt.Errorf("decode Claude guardian config: %w", err)
	}

	if err := validateTurnSupervisorConfig(config); err != nil {
		_, _ = completion.WriteString(turnSupervisorProof)

		return err
	}

	startFD := uintptr(7)
	if config.AuthorityOrigin == turnSupervisorBorrowed {
		startFD = 9
	}

	startInput := turnSupervisorOpenFile(startFD, "claude-turn-supervisor-start-gate")
	if startInput == nil {
		_, _ = completion.WriteString(turnSupervisorProof)

		return errors.New("claude guardian start gate is unavailable")
	}
	defer startInput.Close()

	if err := setTurnSupervisorCloseOnExec(startInput); err != nil {
		_, _ = completion.WriteString(turnSupervisorProof)

		return err
	}

	signals := make(chan os.Signal, 2)

	turnSupervisorSignalNotify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer turnSupervisorSignalStop(signals)

	controlDone := make(chan struct{})

	go func() {
		_, _ = io.Copy(io.Discard, controlFile)

		close(controlDone)
	}()

	authority, err := acquireTurnSupervisorAuthority(config, 7, 8, controlDone, signals)
	if err != nil {
		_, _ = completion.WriteString(turnSupervisorProof)

		return err
	}

	if err = turnSupervisorEnable(); err != nil {
		completionErr := completeTurnSupervisorAuthority(completion, &authority, true)

		return errors.Join(fmt.Errorf("enable Claude guardian privileges: %w", err), completionErr)
	}

	liveness, data, peer, livenessStart, err := startTurnSupervisorLiveness(
		config, controlFile, completion, authority.identity, authority.domain,
	)
	if err != nil {
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, containErr, completionErr)
	}
	defer data.Close()
	defer peer.Close()

	livenessLaunch := &processTreeCommand{startGate: livenessStart}
	defer livenessLaunch.abortStartGate()

	waiter, beginWait := startPausedCommandWait(liveness.Wait)
	beginWait()

	reader := bufio.NewReader(data)

	nativePID, armErr := armTurnSupervisorGuardian(
		completion, readyOutput, startInput, data, reader, liveness, livenessLaunch, waiter, &authority,
	)
	if armErr != nil {
		return armErr
	}

	return superviseTurnSupervisorGuardian(
		completion, peer, reader, liveness, waiter, controlDone, signals, nativePID, &authority,
	)
}

// armTurnSupervisorGuardian drives the readiness handshake: it waits for the
// liveness supervisor to arm, reports armed upstream, waits for the caller's own
// start gate, releases the liveness gate, and reads back the native pid it must
// contain. Every refusal kills the liveness process group, waits for it,
// contains what is left and reports completion, exactly as before.
func armTurnSupervisorGuardian(
	completion *os.File,
	readyOutput io.Writer,
	startInput *os.File,
	data *os.File,
	reader *bufio.Reader,
	liveness *exec.Cmd,
	livenessLaunch *processTreeCommand,
	waiter *commandWait,
	authority **turnSupervisorAuthority,
) (int, error) {
	var err error

	if err = turnSupervisorReadDeadline(data, time.Now().Add(turnSupervisorLivenessReadyWait)); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(err, waitErr, containErr, completionErr)
	}

	line, readyErr := reader.ReadString('\n')
	if readyErr != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(fmt.Errorf("await Claude liveness readiness: %w", readyErr), waitErr, containErr, completionErr)
	}

	// The liveness supervisor reports a refusal on the same frame it would have
	// armed on, so a named refusal arrives here rather than at the readiness
	// frame below. Recognising it keeps the reason attributed to the refusal
	// that produced it instead of reporting it as a protocol violation.
	if failure, failed := strings.CutPrefix(strings.TrimSpace(line), turnSupervisorFailure); failed {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(
			fmt.Errorf("claude liveness failed before readiness: %s", failure), waitErr, containErr, completionErr,
		)
	}

	if line != turnSupervisorArmed {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(fmt.Errorf("invalid Claude liveness armed state %q", strings.TrimSpace(line)), waitErr, containErr, completionErr)
	}

	if _, err = io.WriteString(readyOutput, turnSupervisorArmed); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(err, waitErr, containErr, completionErr)
	}

	if err = readTurnSupervisorStartGate(startInput); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(fmt.Errorf("await Claude guardian start gate: %w", err), waitErr, containErr, completionErr)
	}

	if err = livenessLaunch.releaseStartGate(); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(fmt.Errorf("release Claude liveness start gate: %w", err), waitErr, containErr, completionErr)
	}

	line, readyErr = reader.ReadString('\n')
	if readyErr != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(fmt.Errorf("await Claude liveness readiness: %w", readyErr), waitErr, containErr, completionErr)
	}

	if err = turnSupervisorReadDeadline(data, time.Time{}); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(err, waitErr, containErr, completionErr)
	}

	nativePID, err := parseTurnSupervisorLivenessReady(line)
	if err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(err, waitErr, containErr, completionErr)
	}

	if _, err = io.WriteString(readyOutput, turnSupervisorReady); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), nativePID)
		completionErr := completeTurnSupervisorAuthority(completion, authority, containErr == nil)

		return 0, errors.Join(err, waitErr, containErr, completionErr)
	}

	return nativePID, nil
}

// superviseTurnSupervisorGuardian holds the armed launch until the liveness
// supervisor exits, the control channel closes, or a signal has to be forwarded,
// then adjudicates what the liveness side reported on its way out.
func superviseTurnSupervisorGuardian(
	completion *os.File,
	peer *os.File,
	reader *bufio.Reader,
	liveness *exec.Cmd,
	waiter *commandWait,
	controlDone <-chan struct{},
	signals <-chan os.Signal,
	nativePID int,
	authority **turnSupervisorAuthority,
) error {
	type terminalResult struct {
		line string
		err  error
	}

	terminal := make(chan terminalResult, 1)

	go func() {
		line, err := reader.ReadString('\n')
		terminal <- terminalResult{line: line, err: err}
	}()

	var (
		result  terminalResult
		waitErr error
	)

	for {
		select {
		case result = <-terminal:
			goto terminalReceived
		case <-controlDone:
			// Closing the control channel is the normal request for liveness to
			// contain its native tree. Keep consuming the terminal channel while
			// it does so; waiting for process exit first would deadlock the
			// acknowledged handoff because liveness stays alive until ACK.
			controlDone = nil
		case received := <-signals:
			nativeSignal, signalOK := received.(syscall.Signal)
			if signalOK {
				_ = signalProcessGroupID(liveness.Process.Pid, nativeSignal)
			}
		}
	}

terminalReceived:
	_, terminalErr := parseTurnSupervisorTerminalFrame(result.line)

	if result.err == nil && terminalErr == nil {
		if turnSupervisorAfterTerminalRead != nil {
			turnSupervisorAfterTerminalRead()
		}

		closeErr := closeTurnSupervisorAuthority(authority)
		if closeErr != nil {
			return errors.Join(waitErr, closeErr)
		}

		if turnSupervisorBeforeTerminalACK != nil {
			turnSupervisorBeforeTerminalACK()
		}

		written, ackErr := turnSupervisorWritePeer(peer, []byte(result.line))
		if ackErr == nil && written != len(result.line) {
			ackErr = io.ErrShortWrite
		}

		if ackErr != nil {
			return errors.Join(waitErr, fmt.Errorf("acknowledge Claude liveness completion: %w", ackErr))
		}

		_ = peer.Close()

		if turnSupervisorAfterTerminalACK != nil {
			turnSupervisorAfterTerminalACK()
		}

		if waitErr == nil {
			waitErr, _ = waiter.await(context.Background())
		}

		return waitErr
	}

	if waitErr == nil {
		waitErr, _ = waiter.await(context.Background())
	}

	containErr := turnSupervisorContain(turnSupervisorProcessID(), nativePID)

	if failure, failed := strings.CutPrefix(strings.TrimSpace(result.line), turnSupervisorFailure); failed {
		closeErr := completeTurnSupervisorAuthority(completion, authority, false)

		return errors.Join(waitErr, fmt.Errorf("claude liveness completion failed: %s", failure), result.err, containErr, closeErr)
	}

	result.err = errors.Join(result.err, terminalErr)

	completionErr := completeTurnSupervisorAuthority(
		completion,
		authority,
		containErr == nil && turnSupervisorSignaledExit(waitErr),
	)

	return errors.Join(
		waitErr, fmt.Errorf("claude liveness exited without completion report: %v", result.err), containErr, completionErr,
	)
}

type turnSupervisorAuthority struct {
	identity   *agentIdentityLock
	domain     *agentIdentityLock
	standalone *agentStandaloneIdentity
}

func (authority *turnSupervisorAuthority) Close() error {
	if authority == nil {
		return nil
	}

	if authority.standalone != nil {
		return authority.standalone.Close()
	}

	return errors.Join(authority.identity.Close(), authority.domain.Close())
}

func completeTurnSupervisorAuthority(
	completion io.Writer,
	authority **turnSupervisorAuthority,
	contained bool,
) error {
	if authority == nil || *authority == nil {
		return errors.New("claude guardian authority is unavailable at completion")
	}

	closeErr := (*authority).Close()
	*authority = nil

	if closeErr != nil || !contained {
		return closeErr
	}

	if _, err := io.WriteString(completion, turnSupervisorProof); err != nil {
		return fmt.Errorf("publish Claude guardian completion: %w", err)
	}

	return nil
}

func closeTurnSupervisorAuthority(authority **turnSupervisorAuthority) error {
	if authority == nil || *authority == nil {
		return errors.New("claude guardian authority is unavailable at completion")
	}

	closeErr := (*authority).Close()
	*authority = nil

	return closeErr
}

func turnSupervisorSignaledExit(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)

	return ok && status.Signaled()
}

func acquireTurnSupervisorAuthority(
	config turnSupervisorConfig,
	identityFD uintptr,
	domainFD uintptr,
	canceled <-chan struct{},
	signals <-chan os.Signal,
) (*turnSupervisorAuthority, error) {
	if config.IdentityLock {
		identity, domain, err := adoptTurnSupervisorBorrowedAuthority(config, identityFD, domainFD)
		if err != nil {
			return nil, err
		}

		return &turnSupervisorAuthority{identity: identity, domain: domain}, nil
	}

	standalone, err := turnSupervisorAcquireStandalone(
		config.Isolation.UID,
		config.Isolation.GID,
		config.Isolation.StandaloneOwnerID,
		config.Isolation.StandaloneStateRoot,
		false,
		"",
		canceled,
		signals,
	)
	if err != nil {
		return nil, fmt.Errorf("acquire Claude standalone agent identity authority: %w", err)
	}

	return &turnSupervisorAuthority{
		identity: standalone.identity, domain: standalone.authority, standalone: standalone,
	}, nil
}

func adoptTurnSupervisorBorrowedAuthority(
	config turnSupervisorConfig,
	identityFD uintptr,
	domainFD uintptr,
) (*agentIdentityLock, *agentIdentityLock, error) {
	identity, err := adoptAgentIdentityLock(
		turnSupervisorOpenFile(identityFD, "claude-agent-identity-lock"),
		config.Isolation.UID,
		false,
		"",
	)
	if err != nil {
		return nil, nil, fmt.Errorf("adopt Claude agent identity lock: %w", err)
	}

	domain, err := adoptAgentAuthorityDomain(
		turnSupervisorOpenFile(domainFD, "claude-agent-authority-domain"),
		false,
		"",
	)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("adopt Claude agent authority domain: %w", err), identity.Close())
	}

	if err = validateTurnSupervisorAdoptedAuthorityDisposition(config, false, ""); err != nil {
		return nil, nil, errors.Join(err, identity.Close(), domain.Close())
	}

	return identity, domain, nil
}

func startTurnSupervisorLiveness(
	config turnSupervisorConfig,
	control *os.File,
	completion *os.File,
	identity *agentIdentityLock,
	domain *agentIdentityLock,
) (*exec.Cmd, *os.File, *os.File, *os.File, error) {
	borrowedAuthority := identity != nil && identity.file != nil && domain != nil && domain.file != nil
	config.IdentityLock = borrowedAuthority
	config.AuthorityDomain = borrowedAuthority
	config.Isolation.IdentityLock = nil

	config.Isolation.AuthorityDomain = nil
	if config.AuthorityOrigin == turnSupervisorBorrowed {
		config.Isolation.StandaloneOwnerID = ""
		config.Isolation.StandaloneStateRoot = ""
	}

	configFD, err := turnSupervisorMemfd(turnSupervisorFDName+"-liveness", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	configFile := os.NewFile(uintptr(configFD), turnSupervisorFDName+"-liveness")
	if err = turnSupervisorWriteConfig(configFile, config); err != nil {
		_ = configFile.Close()

		return nil, nil, nil, nil, err
	}

	if _, err = turnSupervisorSealConfig(
		configFile.Fd(), unix.F_ADD_SEALS,
		unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL,
	); err != nil {
		_ = configFile.Close()

		return nil, nil, nil, nil, err
	}

	dataRead, dataWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()

		return nil, nil, nil, nil, err
	}

	peerRead, peerWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()

		return nil, nil, nil, nil, err
	}

	startRead, startWrite, err := turnSupervisorPipe()
	if err != nil {
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()

		return nil, nil, nil, nil, err
	}

	var identityDuplicate *os.File
	if borrowedAuthority {
		identityDuplicate, err = identity.Duplicate()
	} else {
		identityDuplicate, err = os.Open("/dev/null")
	}

	if err != nil {
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()
		_ = startRead.Close()
		_ = startWrite.Close()

		return nil, nil, nil, nil, err
	}

	var domainDuplicate *os.File
	if borrowedAuthority {
		domainDuplicate, err = domain.Duplicate()
	} else {
		domainDuplicate, err = os.Open("/dev/null")
	}

	if err != nil {
		_ = identityDuplicate.Close()
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()
		_ = startRead.Close()
		_ = startWrite.Close()

		return nil, nil, nil, nil, err
	}

	executable, err := turnSupervisorExecutable()
	if err != nil {
		_ = identityDuplicate.Close()
		_ = domainDuplicate.Close()
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()
		_ = startRead.Close()
		_ = startWrite.Close()

		return nil, nil, nil, nil, err
	}

	liveness := turnSupervisorCommand(executable)
	liveness.Dir = "/"
	liveness.Env = turnSupervisorEnvironmentFor(turnSupervisorLivenessMode)
	liveness.Stdin = os.Stdin
	liveness.Stdout = os.Stdout
	liveness.Stderr = os.Stderr
	liveness.ExtraFiles = []*os.File{
		configFile, control, dataWrite, identityDuplicate, domainDuplicate, completion, peerRead, startRead,
	}

	liveness.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = liveness.Start(); err != nil {
		_ = identityDuplicate.Close()
		_ = domainDuplicate.Close()
		_ = configFile.Close()
		_ = dataRead.Close()
		_ = dataWrite.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()
		_ = startRead.Close()
		_ = startWrite.Close()

		return nil, nil, nil, nil, err
	}

	for _, file := range []*os.File{configFile, dataWrite, identityDuplicate, domainDuplicate, peerRead, startRead} {
		_ = file.Close()
	}

	return liveness, dataRead, peerWrite, startWrite, nil
}

func readTurnSupervisorStartGate(input io.Reader) error {
	var token [1]byte
	if _, err := io.ReadFull(input, token[:]); err != nil {
		return err
	}

	if token[0] != 1 {
		return fmt.Errorf("invalid start gate token %d", token[0])
	}

	return nil
}

func parseTurnSupervisorLivenessReady(line string) (int, error) {
	text, ok := strings.CutSuffix(line, "\n")
	if !ok {
		return 0, errors.New("claude liveness readiness is not newline terminated")
	}

	if failure, failed := strings.CutPrefix(text, turnSupervisorFailure); failed {
		return 0, fmt.Errorf("claude liveness failed before readiness: %s", failure)
	}

	pidText, ok := strings.CutPrefix(text, "ready:")
	if !ok {
		return 0, fmt.Errorf("invalid Claude liveness readiness %q", text)
	}

	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid Claude liveness native pid %q", pidText)
	}

	return pid, nil
}

func validateTurnSupervisorConfig(config turnSupervisorConfig) error {
	if config.Path == "" || len(config.Args) == 0 {
		return errors.New("claude native supervisor config is incomplete")
	}

	if config.IdentityLock != config.AuthorityDomain {
		return errors.New("claude native supervisor identity lock and authority domain must be provided together")
	}

	switch config.AuthorityOrigin {
	case turnSupervisorBorrowed:
		if !config.IdentityLock || config.Isolation.StandaloneOwnerID != "" || config.Isolation.StandaloneStateRoot != "" {
			return errors.New("claude borrowed supervisor authority disposition is invalid")
		}
	case turnSupervisorStandalone:
		if !validStandaloneOwnerID(config.Isolation.StandaloneOwnerID) ||
			!validStandaloneStateRootPath(config.Isolation.StandaloneStateRoot) {
			return errors.New("claude standalone supervisor authority disposition is invalid")
		}
	default:
		return errors.New("claude native supervisor authority origin is invalid")
	}

	validation := config.Isolation
	if config.IdentityLock && config.AuthorityOrigin == turnSupervisorBorrowed {
		placeholder := &agentIdentityLock{}
		validation.IdentityLock = placeholder
		validation.AuthorityDomain = placeholder
	}

	if err := validateProcessIsolation(&validation); err != nil {
		return fmt.Errorf("validate Claude native supervisor isolation: %w", err)
	}

	if err := validateTurnSupervisorIdentity(&config.Isolation); err != nil {
		return fmt.Errorf("validate Claude native supervisor identity: %w", err)
	}

	return nil
}

func runTurnSupervisorLiveness(configInput io.Reader, controlInput io.Reader, readyOutput io.Writer) error {
	completion := turnSupervisorOpenFile(8, "claude-turn-supervisor-completion")
	peer := turnSupervisorOpenFile(9, "claude-turn-supervisor-guardian-peer")

	startInput := turnSupervisorOpenFile(10, "claude-turn-supervisor-start-gate")
	if completion == nil || peer == nil || startInput == nil {
		if completion != nil {
			_ = completion.Close()
		}

		if peer != nil {
			_ = peer.Close()
		}

		if startInput != nil {
			_ = startInput.Close()
		}

		return errors.New("claude liveness inherited descriptors are unavailable")
	}

	defer completion.Close()
	defer peer.Close()
	defer startInput.Close()

	if err := setTurnSupervisorCloseOnExec(completion); err != nil {
		return err
	}

	if err := setTurnSupervisorCloseOnExec(peer); err != nil {
		return err
	}

	if err := setTurnSupervisorCloseOnExec(startInput); err != nil {
		return err
	}

	return runTurnSupervisorNative(
		configInput, []io.Reader{controlInput}, peer, startInput, readyOutput, completion, 6, 7,
	)
}

func runTurnSupervisorNative(
	configInput io.Reader,
	controlInputs []io.Reader,
	guardianPeer *os.File,
	startInput io.Reader,
	readyOutput io.Writer,
	completionOutput io.Writer,
	identityFD uintptr,
	authorityFD uintptr,
) (runErr error) {
	var config turnSupervisorConfig
	if err := json.NewDecoder(configInput).Decode(&config); err != nil {
		return fmt.Errorf("decode Claude native supervisor config: %w", err)
	}

	if err := validateTurnSupervisorConfig(config); err != nil {
		return err
	}

	signals := make(chan os.Signal, 2)

	// A stopped liveness group receives SIGHUP when guardian death orphans it.
	// Keep that event inside the normal forwarding and containment path.
	turnSupervisorSignalNotify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer turnSupervisorSignalStop(signals)

	controlDone := make(chan struct{})

	var controlOnce sync.Once

	for _, controlInput := range controlInputs {
		go func(input io.Reader) {
			_, _ = io.Copy(io.Discard, input)

			controlOnce.Do(func() { close(controlDone) })
		}(controlInput)
	}

	guardianState := newTurnSupervisorGuardianState(guardianPeer)

	if guardianState != nil {
		go func() {
			guardianState.observe()

			if guardianState.err != nil {
				controlOnce.Do(func() { close(controlDone) })
			}
		}()
	}

	var (
		identityLock    *agentIdentityLock
		authorityDomain *agentIdentityLock
		standalone      *agentStandaloneIdentity
		err             error
	)

	if config.IdentityLock {
		identityLock, authorityDomain, err = adoptTurnSupervisorBorrowedAuthority(config, identityFD, authorityFD)
		if err != nil {
			return err
		}
	} else {
		standalone, err = turnSupervisorAcquireStandalone(
			config.Isolation.UID,
			config.Isolation.GID,
			config.Isolation.StandaloneOwnerID,
			config.Isolation.StandaloneStateRoot,
			false,
			"",
			controlDone,
			signals,
		)
		if err != nil {
			return fmt.Errorf("acquire Claude standalone agent identity authority: %w", err)
		}

		identityLock = standalone.identity
		authorityDomain = standalone.authority
	}

	if identityLock == nil || authorityDomain == nil {
		return errors.New("claude agent identity authority is incomplete")
	}

	contained := false

	guardianExited := false
	defer func() {
		runErr = errors.Join(runErr, completeTurnSupervisorLiveness(
			standalone,
			identityLock,
			authorityDomain,
			contained,
			guardianExited,
			guardianState,
			readyOutput,
			completionOutput,
		))
	}()

	native := turnSupervisorCommand(config.Path, config.Args[1:]...) // #nosec G204 -- private config was built from the operator-selected Claude command.
	native.Args = append([]string(nil), config.Args...)
	native.Dir = config.Dir
	native.Env = append([]string(nil), config.Env...)
	native.Stdin = os.Stdin
	native.Stdout = os.Stdout
	native.Stderr = os.Stderr
	configureProcessCommand(native)

	nativeIsolation := config.Isolation
	nativeIsolation.StandaloneOwnerID = ""
	nativeIsolation.StandaloneStateRoot = ""
	nativeIsolation.IdentityLock = nil
	nativeIsolation.AuthorityDomain = nil
	nativeIsolation.identityAuthorityAdopted = true

	if err := validateTurnSupervisorGuardianPeer(guardianPeer, guardianState); err != nil {
		guardianExited = errors.Is(err, errTurnSupervisorGuardianExited)
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		contained = containErr == nil

		return errors.Join(err, containErr)
	}

	if _, err := io.WriteString(readyOutput, turnSupervisorArmed); err != nil {
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		contained = containErr == nil

		return errors.Join(fmt.Errorf("publish Claude liveness armed state: %w", err), containErr)
	}

	if err := readTurnSupervisorStartGate(startInput); err != nil {
		peerErr := validateTurnSupervisorGuardianPeer(guardianPeer, guardianState)
		guardianExited = errors.Is(peerErr, errTurnSupervisorGuardianExited)
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		contained = containErr == nil

		return errors.Join(fmt.Errorf("claude guardian start gate closed before native launch: %w", err), peerErr, containErr)
	}

	var finalPeerErr error

	waitDone, enableErr, startErr := startTurnSupervisorNative(native, &nativeIsolation, func() error {
		finalPeerErr = validateTurnSupervisorGuardianPeer(guardianPeer, guardianState)

		return finalPeerErr
	})
	if enableErr != nil {
		return fmt.Errorf("enable Claude native supervisor privileges: %w", enableErr)
	}

	if finalPeerErr != nil {
		guardianExited = errors.Is(finalPeerErr, errTurnSupervisorGuardianExited)
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		contained = containErr == nil

		return errors.Join(finalPeerErr, containErr)
	}

	if startErr != nil {
		return fmt.Errorf("start supervised Claude native root: %w", startErr)
	}

	ready := fmt.Sprintf("ready:%d\n", native.Process.Pid)
	if _, err := io.WriteString(readyOutput, ready); err != nil {
		_ = turnSupervisorSignalGroup(native.Process.Pid, syscall.SIGKILL)
		waitErr := <-waitDone
		containErr := turnSupervisorContain(turnSupervisorProcessID(), native.Process.Pid)
		contained = containErr == nil

		return errors.Join(fmt.Errorf("publish Claude native supervisor readiness: %w", err), containErr, waitErr)
	}

	for {
		select {
		case waitErr := <-waitDone:
			if err := turnSupervisorContain(turnSupervisorProcessID(), native.Process.Pid); err != nil {
				return err
			}

			contained = true

			return waitErr
		case <-controlDone:
			guardianExited = errors.Is(
				validateTurnSupervisorGuardianPeer(guardianPeer, guardianState),
				errTurnSupervisorGuardianExited,
			)
			_ = turnSupervisorSignalGroup(native.Process.Pid, syscall.SIGKILL)
			waitErr := <-waitDone

			if err := turnSupervisorContain(turnSupervisorProcessID(), native.Process.Pid); err != nil {
				return err
			}

			contained = true

			return waitErr
		case received := <-signals:
			nativeSignal, ok := received.(syscall.Signal)
			if !ok {
				continue
			}

			if nativeSignal == syscall.SIGHUP {
				// Linux sends SIGHUP followed by SIGCONT when guardian death
				// orphans a stopped liveness process group. Guardian-peer EOF,
				// not this unauthenticated signal, authorizes containment.
				continue
			}

			_ = turnSupervisorSignalGroup(native.Process.Pid, nativeSignal)
		}
	}
}

func completeTurnSupervisorLiveness(
	standalone *agentStandaloneIdentity,
	identityLock *agentIdentityLock,
	authorityDomain *agentIdentityLock,
	contained bool,
	guardianExited bool,
	guardianState *turnSupervisorGuardianState,
	readyOutput io.Writer,
	completionOutput io.Writer,
) error {
	var closeErr error
	if standalone != nil {
		closeErr = standalone.Close()
	} else {
		closeErr = errors.Join(identityLock.Close(), authorityDomain.Close())
	}

	if closeErr != nil {
		_, reportErr := fmt.Fprintf(readyOutput, "%sclose Claude liveness authority: %v\n", turnSupervisorFailure, closeErr)

		return errors.Join(closeErr, reportErr)
	}

	if !contained {
		return nil
	}

	if guardianExited {
		if _, err := io.WriteString(completionOutput, turnSupervisorProof); err != nil {
			return fmt.Errorf("publish Claude liveness completion: %w", err)
		}

		return nil
	}

	if guardianState == nil {
		return errors.New("claude liveness guardian acknowledgement is unavailable")
	}

	if guardianState.preTerminalErr != nil {
		return guardianState.preTerminalErr
	}

	select {
	case <-guardianState.done:
		if guardianState.err == nil {
			return errors.New("claude guardian sent a terminal response before the terminal report")
		}

		if errors.Is(guardianState.err, io.EOF) {
			return publishTurnSupervisorLivenessProof(completionOutput)
		}

		return guardianState.err
	default:
	}

	terminalFrame, err := newTurnSupervisorTerminalFrame()
	if err != nil {
		return err
	}

	written, err := io.WriteString(readyOutput, terminalFrame)
	if err == nil && written != len(terminalFrame) {
		err = io.ErrShortWrite
	}

	if err != nil {
		if errors.Is(err, syscall.EPIPE) {
			<-guardianState.done

			if errors.Is(guardianState.err, io.EOF) {
				return publishTurnSupervisorLivenessProof(completionOutput)
			}
		}

		return fmt.Errorf("publish Claude liveness terminal result: %w", err)
	}

	<-guardianState.done

	if guardianState.err != nil && !errors.Is(guardianState.err, io.EOF) {
		return guardianState.err
	}

	if guardianState.err == nil && guardianState.response != terminalFrame {
		return errors.New("claude guardian completion acknowledgement did not echo the terminal challenge")
	}

	return publishTurnSupervisorLivenessProof(completionOutput)
}

func newTurnSupervisorTerminalFrame() (string, error) {
	var challenge [turnSupervisorChallengeLen]byte
	if _, err := io.ReadFull(turnSupervisorChallengeSource, challenge[:]); err != nil {
		return "", fmt.Errorf("generate Claude liveness terminal challenge: %w", err)
	}

	return turnSupervisorDonePrefix + hex.EncodeToString(challenge[:]) + "\n", nil
}

func parseTurnSupervisorTerminalFrame(frame string) (string, error) {
	if len(frame) != turnSupervisorTerminalLen || !strings.HasPrefix(frame, turnSupervisorDonePrefix) || frame[len(frame)-1] != '\n' {
		return "", fmt.Errorf("invalid Claude liveness terminal frame %q", strings.TrimSpace(frame))
	}

	encoded := frame[len(turnSupervisorDonePrefix) : len(frame)-1]

	challenge, err := hex.DecodeString(encoded)
	if err != nil || len(challenge) != turnSupervisorChallengeLen {
		return "", fmt.Errorf("invalid Claude liveness terminal challenge %q", encoded)
	}

	return encoded, nil
}

func publishTurnSupervisorLivenessProof(completionOutput io.Writer) error {
	if _, err := io.WriteString(completionOutput, turnSupervisorProof); err != nil {
		return fmt.Errorf("publish Claude liveness completion: %w", err)
	}

	return nil
}

type turnSupervisorGuardianState struct {
	peer           *os.File
	done           chan struct{}
	response       string
	err            error
	preTerminalErr error
}

func newTurnSupervisorGuardianState(peer *os.File) *turnSupervisorGuardianState {
	if peer == nil {
		return nil
	}

	return &turnSupervisorGuardianState{peer: peer, done: make(chan struct{})}
}

func (state *turnSupervisorGuardianState) observe() {
	response, err := io.ReadAll(io.LimitReader(state.peer, int64(turnSupervisorTerminalLen+1)))
	switch {
	case err != nil:
	case len(response) == 0:
		err = io.EOF
	case len(response) != turnSupervisorTerminalLen:
		err = fmt.Errorf("invalid Claude guardian completion acknowledgement length %d", len(response))
	default:
		state.response = string(response)
		if _, parseErr := parseTurnSupervisorTerminalFrame(state.response); parseErr != nil {
			err = fmt.Errorf("invalid Claude guardian completion acknowledgement: %v", parseErr)
		}
	}

	state.err = err
	close(state.done)
}

func validateTurnSupervisorGuardianPeer(peer *os.File, state *turnSupervisorGuardianState) error {
	if peer == nil {
		return nil
	}

	if state == nil {
		return errors.New("claude guardian peer state is unavailable")
	}

	select {
	case <-state.done:
		return observedTurnSupervisorGuardianPeerState(state)
	default:
	}

	poll := []unix.PollFd{{
		Fd:     pollFD(peer),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}

	ready, err := turnSupervisorPoll(poll, 0)
	if err != nil {
		state.preTerminalErr = fmt.Errorf("poll Claude guardian before native launch: %w", err)

		return state.preTerminalErr
	}

	if ready == 0 && poll[0].Revents == 0 {
		return nil
	}

	if poll[0].Revents&unix.POLLHUP != 0 {
		// A hangup makes the observer's bounded read finite. Let that single
		// reader distinguish empty EOF (guardian death) from queued bytes plus
		// EOF (a protocol fault); readiness alone proves neither.
		<-state.done

		return observedTurnSupervisorGuardianPeerState(state)
	}

	state.preTerminalErr = fmt.Errorf(
		"claude guardian peer became readable before native launch without confirmed empty EOF (events %#x)",
		poll[0].Revents,
	)

	return state.preTerminalErr
}

func observedTurnSupervisorGuardianPeerState(state *turnSupervisorGuardianState) error {
	if errors.Is(state.err, io.EOF) {
		return errTurnSupervisorGuardianExited
	}

	if state.err == nil {
		return errors.New("claude guardian acknowledged completion before native launch")
	}

	return state.err
}

func validateTurnSupervisorIdentity(isolation *ProcessIsolation) error {
	if isolation == nil {
		return errors.New("process isolation is required")
	}

	// The supervisor drops privilege to reach the native identity, so it has to
	// hold a higher one first. A supplied policy is a strict selection: it is
	// refused here rather than degraded, even when its ids happen to name the
	// identity the caller already holds.
	effectiveUID := turnSupervisorEffectiveUID()
	if effectiveUID != 0 {
		return fmt.Errorf("trusted root identity is required, effective uid is %d", effectiveUID)
	}

	if isolation.UID == uint32(effectiveUID) {
		return errors.New("native target identity must differ from the trusted supervisor")
	}

	return nil
}

// awaitLinuxSupervisorContainment never lets the dedicated subreaper exit on
// an incomplete tree. The adapter retains the managed-root permit when its bounded
// parent-side wait expires; meanwhile the helper keeps retrying until it can
// truthfully publish completion by exiting.
func awaitLinuxSupervisorContainment(supervisorPID int, nativePID int) error {
	for {
		err := containLinuxSupervisorDescendants(supervisorPID, nativePID)
		if err == nil {
			return nil
		}

		turnSupervisorSleep(time.Second)
	}
}

func containLinuxSupervisorDescendants(supervisorPID int, nativePID int) error {
	if nativePID > 0 {
		_ = turnSupervisorSignalGroup(nativePID, syscall.SIGKILL)
	}

	for {
		waited, waitErr := turnSupervisorWait4(-1, nil, unix.WNOHANG, nil)
		switch {
		case waited > 0:
			continue
		case errors.Is(waitErr, unix.EINTR):
			continue
		case errors.Is(waitErr, unix.ECHILD):
			return nil
		case waitErr != nil:
			return fmt.Errorf("%w: reap supervised Claude descendants: %v", ErrProcessContainmentIncomplete, waitErr)
		case waited < 0:
			return fmt.Errorf("%w: invalid supervised Claude wait result %d", ErrProcessContainmentIncomplete, waited)
		}

		descendants, err := turnSupervisorDescendants(supervisorPID)
		if err != nil {
			return fmt.Errorf("%w: enumerate supervised Claude descendants: %v", ErrProcessContainmentIncomplete, err)
		}

		for _, descendant := range descendants {
			if descendant.state != 'Z' {
				if err := turnSupervisorSignalPID(descendant, syscall.SIGKILL); err != nil {
					return fmt.Errorf("%w: kill supervised Claude descendant %d: %v", ErrProcessContainmentIncomplete, descendant.pid, err)
				}
			}
		}

		turnSupervisorSleep(5 * time.Millisecond)
	}
}

func linuxDescendants(rootPID int) ([]linuxProcessIdentity, error) {
	entries, err := os.ReadDir(turnSupervisorProcRoot)
	if err != nil {
		return nil, err
	}

	children := make(map[int][]linuxProcessIdentity)

	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}

		identity, readErr := turnSupervisorIdentity(pid)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}

		if readErr != nil {
			return nil, readErr
		}

		children[identity.parentPID] = append(children[identity.parentPID], identity)
	}

	result := make([]linuxProcessIdentity, 0)

	queue := append([]linuxProcessIdentity(nil), children[rootPID]...)
	for len(queue) > 0 {
		identity := queue[0]
		queue = queue[1:]

		result = append(result, identity)
		queue = append(queue, children[identity.pid]...)
	}

	return result, nil
}

func readLinuxProcessIdentity(pid int) (linuxProcessIdentity, error) {
	raw, err := os.ReadFile(filepath.Join(turnSupervisorProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return linuxProcessIdentity{}, err
	}

	line := string(raw)

	closing := strings.LastIndexByte(line, ')')
	if closing < 0 || closing+2 >= len(line) {
		return linuxProcessIdentity{}, fmt.Errorf("parse /proc/%d/stat: malformed comm field", pid)
	}

	fields := strings.Fields(line[closing+2:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return linuxProcessIdentity{}, fmt.Errorf("parse /proc/%d/stat: incomplete fields", pid)
	}

	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return linuxProcessIdentity{}, fmt.Errorf("parse /proc/%d/stat parent: %w", pid, err)
	}

	groupID, err := strconv.Atoi(fields[2])
	if err != nil {
		return linuxProcessIdentity{}, fmt.Errorf("parse /proc/%d/stat group: %w", pid, err)
	}

	return linuxProcessIdentity{
		pid:       pid,
		parentPID: parentPID,
		groupID:   groupID,
		state:     fields[0][0],
		startTime: fields[19],
	}, nil
}

func signalLinuxIdentity(identity linuxProcessIdentity, processSignal syscall.Signal) error {
	current, err := turnSupervisorIdentity(identity.pid)
	if errors.Is(err, os.ErrNotExist) || (err == nil && current.startTime != identity.startTime) {
		return nil
	}

	if err != nil {
		return err
	}

	if err := syscallKill(identity.pid, processSignal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

// Seam for the fail-closed guard in pollFD. Linux hands out small descriptors,
// so the guard is unreachable through a real *os.File; tests swap this to reach it.
var pollFDSource = (*os.File).Fd

// pollFD narrows a descriptor to the int32 unix.PollFd carries. Linux hands out
// small non-negative descriptors, so the guard never fires; when the value
// cannot be represented it yields -1, which poll reports as EBADF rather than
// aliasing onto a live descriptor.
func pollFD(file *os.File) int32 {
	fd := pollFDSource(file)
	if fd > math.MaxInt32 {
		return -1
	}

	return int32(fd)
}
