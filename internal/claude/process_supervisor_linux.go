//go:build linux

package claude

import (
	"bufio"
	"context"
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
	turnSupervisorFailure      = "error:"
	turnSupervisorBorrowed     = "borrowed"
	turnSupervisorStandalone   = "standalone"
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
	errTurnSupervisorGuardianExited = errors.New("Claude guardian exited before native launch")

	turnSupervisorExecutable        = os.Executable
	turnSupervisorMemfd             = unix.MemfdCreate
	turnSupervisorPipe              = os.Pipe
	turnSupervisorExit              = os.Exit
	turnSupervisorSignalNotify      = signal.Notify
	turnSupervisorSignalStop        = signal.Stop
	turnSupervisorEnable            = enableTurnSupervisor
	turnSupervisorNoNewPrivs        = enableTurnSupervisorNoNewPrivileges
	turnSupervisorCoreLimit         = disableTurnSupervisorCoreDumps
	turnSupervisorCommand           = exec.Command
	turnSupervisorContain           = awaitLinuxSupervisorContainment
	turnSupervisorProcessID         = os.Getpid
	turnSupervisorSignalGroup       = signalProcessGroupID
	turnSupervisorWriteConfig       = writeTurnSupervisorConfig
	turnSupervisorDescendants       = linuxDescendants
	turnSupervisorIdentity          = readLinuxProcessIdentity
	turnSupervisorSignalPID         = signalLinuxIdentity
	turnSupervisorWait4             = unix.Wait4
	turnSupervisorSleep             = time.Sleep
	turnSupervisorProcRoot          = "/proc"
	turnSupervisorRun               = runTurnSupervisorGuardian
	turnSupervisorRunLiveness       = runTurnSupervisorLiveness
	turnSupervisorOpenFile          = os.NewFile
	turnSupervisorFcntl             = unix.FcntlInt
	turnSupervisorInput             = inheritedTurnSupervisorInput
	turnSupervisorPrctl             = unix.Prctl
	turnSupervisorSetrlimit         = unix.Setrlimit
	turnSupervisorAcquireStandalone = acquireAgentStandaloneIdentity
	turnSupervisorSealConfig        = unix.FcntlInt
	turnSupervisorEffectiveUID      = os.Geteuid
	turnSupervisorPoll              = unix.Poll
	turnSupervisorReadDeadline      = (*os.File).SetReadDeadline
	turnSupervisorBeforeRelease     func(*os.Process) error
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

	if err := launch.ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("arm Claude native supervisor readiness: %w", err)
	}

	reader := bufio.NewReader(launch.ready)

	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("await Claude native supervisor readiness: %w", err)
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
		return fmt.Errorf("Claude native supervisor failed before readiness: %s", failure)
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
		return errors.New("Claude guardian completion descriptor is unavailable")
	}
	defer completion.Close()

	if err := setTurnSupervisorCloseOnExec(completion); err != nil {
		return err
	}

	controlFile, ok := controlInput.(*os.File)
	if !ok {
		_, _ = completion.WriteString(turnSupervisorProof)

		return errors.New("Claude guardian control input is not an inheritable file")
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

		return errors.New("Claude guardian start gate is unavailable")
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
	if err = turnSupervisorReadDeadline(data, time.Now().Add(5*time.Second)); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, waitErr, containErr, completionErr)
	}

	line, readyErr := reader.ReadString('\n')
	if readyErr != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(fmt.Errorf("await Claude liveness readiness: %w", readyErr), waitErr, containErr, completionErr)
	}

	if line != turnSupervisorArmed {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(fmt.Errorf("invalid Claude liveness armed state %q", strings.TrimSpace(line)), waitErr, containErr, completionErr)
	}

	if _, err = io.WriteString(readyOutput, turnSupervisorArmed); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, waitErr, containErr, completionErr)
	}

	if err = readTurnSupervisorStartGate(startInput); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(fmt.Errorf("await Claude guardian start gate: %w", err), waitErr, containErr, completionErr)
	}

	if err = livenessLaunch.releaseStartGate(); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(fmt.Errorf("release Claude liveness start gate: %w", err), waitErr, containErr, completionErr)
	}

	line, readyErr = reader.ReadString('\n')
	if readyErr != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(fmt.Errorf("await Claude liveness readiness: %w", readyErr), waitErr, containErr, completionErr)
	}

	if err = turnSupervisorReadDeadline(data, time.Time{}); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, waitErr, containErr, completionErr)
	}

	nativePID, err := parseTurnSupervisorLivenessReady(line)
	if err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, waitErr, containErr, completionErr)
	}

	if _, err = io.WriteString(readyOutput, turnSupervisorReady); err != nil {
		_ = signalProcessGroupID(liveness.Process.Pid, syscall.SIGKILL)
		waitErr, _ := waiter.await(context.Background())
		containErr := turnSupervisorContain(turnSupervisorProcessID(), nativePID)
		completionErr := completeTurnSupervisorAuthority(completion, &authority, containErr == nil)

		return errors.Join(err, waitErr, containErr, completionErr)
	}

	var waitErr error

	for {
		select {
		case <-waiter.done:
			waitErr = waiter.err

			goto livenessExited
		case <-controlDone:
			waitErr, _ = waiter.await(context.Background())

			goto livenessExited
		case received := <-signals:
			nativeSignal, signalOK := received.(syscall.Signal)
			if signalOK {
				_ = signalProcessGroupID(liveness.Process.Pid, nativeSignal)
			}
		}
	}

livenessExited:
	doneLine, doneErr := reader.ReadString('\n')

	if doneErr == nil && doneLine == "done\n" {
		completionErr := completeTurnSupervisorAuthority(completion, &authority, true)

		return errors.Join(waitErr, completionErr)
	}

	containErr := turnSupervisorContain(turnSupervisorProcessID(), nativePID)

	if failure, failed := strings.CutPrefix(strings.TrimSpace(doneLine), turnSupervisorFailure); failed {
		closeErr := completeTurnSupervisorAuthority(completion, &authority, false)

		return errors.Join(waitErr, fmt.Errorf("Claude liveness completion failed: %s", failure), doneErr, containErr, closeErr)
	}

	completionErr := completeTurnSupervisorAuthority(
		completion,
		&authority,
		containErr == nil && turnSupervisorSignaledExit(waitErr),
	)

	return errors.Join(waitErr, fmt.Errorf("Claude liveness exited without completion report: %v", doneErr), containErr, completionErr)
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
		return errors.New("Claude guardian authority is unavailable at completion")
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
		return 0, errors.New("Claude liveness readiness is not newline terminated")
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
		return errors.New("Claude native supervisor identity lock and authority domain must be provided together")
	}

	switch config.AuthorityOrigin {
	case turnSupervisorBorrowed:
		if !config.IdentityLock || config.Isolation.StandaloneOwnerID != "" || config.Isolation.StandaloneStateRoot != "" {
			return errors.New("Claude borrowed supervisor authority disposition is invalid")
		}
	case turnSupervisorStandalone:
		if !validStandaloneOwnerID(config.Isolation.StandaloneOwnerID) ||
			!validStandaloneStateRootPath(config.Isolation.StandaloneStateRoot) {
			return errors.New("Claude standalone supervisor authority disposition is invalid")
		}
	default:
		return errors.New("Claude native supervisor authority origin is invalid")
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

		return errors.New("Claude liveness inherited descriptors are unavailable")
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

	turnSupervisorSignalNotify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer turnSupervisorSignalStop(signals)

	controlDone := make(chan struct{})

	var controlOnce sync.Once

	for _, controlInput := range controlInputs {
		go func(input io.Reader) {
			_, _ = io.Copy(io.Discard, input)

			controlOnce.Do(func() { close(controlDone) })
		}(controlInput)
	}

	guardianDone := make(chan struct{})

	if guardianPeer != nil {
		go func() {
			_, _ = io.Copy(io.Discard, guardianPeer)

			close(guardianDone)
			controlOnce.Do(func() { close(controlDone) })
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
		return errors.New("Claude agent identity authority is incomplete")
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
			guardianDone,
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

	if err := validateTurnSupervisorGuardianPeer(guardianPeer, guardianDone); err != nil {
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
		peerErr := validateTurnSupervisorGuardianPeer(guardianPeer, guardianDone)
		guardianExited = errors.Is(peerErr, errTurnSupervisorGuardianExited)
		containErr := turnSupervisorContain(turnSupervisorProcessID(), 0)
		contained = containErr == nil

		return errors.Join(fmt.Errorf("Claude guardian start gate closed before native launch: %w", err), peerErr, containErr)
	}

	var finalPeerErr error

	waitDone, enableErr, startErr := startTurnSupervisorNative(native, &nativeIsolation, func() error {
		finalPeerErr = validateTurnSupervisorGuardianPeer(guardianPeer, guardianDone)

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
	guardianDone <-chan struct{},
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

	select {
	case <-guardianDone:
		guardianExited = true
	default:
	}

	if guardianExited {
		if _, err := io.WriteString(completionOutput, turnSupervisorProof); err != nil {
			return fmt.Errorf("publish Claude liveness completion: %w", err)
		}
	} else {
		if _, err := io.WriteString(readyOutput, "done\n"); err != nil {
			return fmt.Errorf("publish Claude liveness terminal result: %w", err)
		}
	}

	return nil
}

func validateTurnSupervisorGuardianPeer(peer *os.File, done <-chan struct{}) error {
	if peer == nil {
		return nil
	}

	select {
	case <-done:
		return errTurnSupervisorGuardianExited
	default:
	}

	poll := []unix.PollFd{{
		Fd:     pollFD(peer),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}

	ready, err := turnSupervisorPoll(poll, 0)
	if err != nil {
		return fmt.Errorf("poll Claude guardian before native launch: %w", err)
	}

	if ready != 0 || poll[0].Revents != 0 {
		return errTurnSupervisorGuardianExited
	}

	return nil
}

func validateTurnSupervisorIdentity(isolation *ProcessIsolation) error {
	if isolation == nil {
		return errors.New("process isolation is required")
	}

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
