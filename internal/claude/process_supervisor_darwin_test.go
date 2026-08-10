//go:build darwin

package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type darwinTestReadCloser struct {
	io.Reader
	closeErr error
	closed   bool
}

type darwinTestWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (w *darwinTestWriteCloser) Close() error {
	w.closed = true

	return nil
}

func (r *darwinTestReadCloser) Close() error {
	r.closed = true

	return r.closeErr
}

func useDarwinTestSupervisorIdentity(t *testing.T) {
	t.Helper()
	originalUID := processEffectiveUID
	originalGID := processEffectiveGID
	t.Setenv(processIsolationUIDEnv, "41")
	t.Setenv(processIsolationGIDEnv, "42")
	processEffectiveUID = func() int { return 41 }
	processEffectiveGID = func() int { return 42 }
	t.Cleanup(func() {
		processEffectiveUID = originalUID
		processEffectiveGID = originalGID
	})
}

func TestConfigureCommandDarwin(t *testing.T) {
	cmd := exec.Command("true")
	configureProcessCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid", cmd.SysProcAttr)
	}
}

// TestDarwinLaunchFailsClosedForExplicitProcessIsolation proves Darwin refuses a
// supplied hardened policy before any spawn — with and without the best-effort
// opt-in, which is mutually exclusive with it — and that omission instead
// selects the ordinary directly-owned boundary with no guardian handshake.
func TestDarwinLaunchFailsClosedForExplicitProcessIsolation(t *testing.T) {
	for _, bestEffort := range []bool{false, true} {
		launch, err := prepareProcessTreeCommand(exec.Command("true"), processLaunchOptions{
			DarwinBestEffort: bestEffort,
			Generation:       &DarwinGeneration{ScratchRoot: t.TempDir()},
			Isolation:        testProcessIsolation(),
		})
		if launch != nil || !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("bestEffort=%v launch = %#v, error = %v", bestEffort, launch, err)
		}
	}

	ordinary, err := prepareProcessTreeCommand(exec.Command("true"), processLaunchOptions{})
	if err != nil || ordinary == nil || !ordinary.ordinary {
		t.Fatalf("ordinary launch = %#v, error = %v", ordinary, err)
	}
	if ordinary.startGate != nil || ordinary.control != nil || ordinary.ready != nil ||
		ordinary.proof != nil || len(ordinary.inherited) != 0 || ordinary.generation != nil {
		t.Fatalf("ordinary launch armed a supervisor handshake: %#v", ordinary)
	}
	if ordinary.cmd.SysProcAttr != nil && ordinary.cmd.SysProcAttr.Credential != nil {
		t.Fatalf("ordinary launch applied a credential: %#v", ordinary.cmd.SysProcAttr.Credential)
	}
}

func TestDarwinLaunchBootstrapProtocol(t *testing.T) {
	useDarwinTestSupervisorIdentity(t)
	originalExec := darwinLaunchExec
	t.Cleanup(func() { darwinLaunchExec = originalExec })
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	configBytes, err := json.Marshal(darwinLaunchConfig{Path: "/native/claude", Args: []string{"claude", "version"}, Dir: workingDirectory, Env: []string{"A=B"}})
	if err != nil {
		t.Fatal(err)
	}
	processEffectiveUID = func() int { return 99 }
	if err := runDarwinLaunchBootstrap(nil, nil); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("bootstrap identity error = %v", err)
	}
	processEffectiveUID = func() int { return 41 }

	config := &darwinTestReadCloser{Reader: bytes.NewReader(configBytes)}
	gate := &darwinTestReadCloser{Reader: bytes.NewReader([]byte{1})}
	var got darwinLaunchConfig
	darwinLaunchExec = func(path string, args []string, env []string) error {
		got = darwinLaunchConfig{Path: path, Args: args, Env: env}

		return nil
	}
	if err := runDarwinLaunchBootstrap(config, gate); err != nil {
		t.Fatal(err)
	}
	if !config.closed || !gate.closed || got.Path != "/native/claude" || len(got.Args) != 2 || len(got.Env) != 1 {
		t.Fatalf("closed=(%v,%v), exec=%#v", config.closed, gate.closed, got)
	}

	for _, test := range []struct {
		name   string
		config *darwinTestReadCloser
		gate   *darwinTestReadCloser
		exec   func(string, []string, []string) error
	}{
		{name: "decode", config: &darwinTestReadCloser{Reader: strings.NewReader("{")}, gate: &darwinTestReadCloser{Reader: strings.NewReader("\x01")}},
		{name: "incomplete", config: &darwinTestReadCloser{Reader: strings.NewReader(`{}`)}, gate: &darwinTestReadCloser{Reader: strings.NewReader("\x01")}},
		{name: "gate eof", config: &darwinTestReadCloser{Reader: bytes.NewReader(configBytes)}, gate: &darwinTestReadCloser{Reader: strings.NewReader("")}},
		{name: "gate byte", config: &darwinTestReadCloser{Reader: bytes.NewReader(configBytes)}, gate: &darwinTestReadCloser{Reader: strings.NewReader("x")}},
		{name: "close", config: &darwinTestReadCloser{Reader: bytes.NewReader(configBytes), closeErr: errors.New("close config")}, gate: &darwinTestReadCloser{Reader: strings.NewReader("\x01")}},
		{name: "exec", config: &darwinTestReadCloser{Reader: bytes.NewReader(configBytes)}, gate: &darwinTestReadCloser{Reader: strings.NewReader("\x01")}, exec: func(string, []string, []string) error { return errors.New("exec") }},
		{name: "chdir", config: &darwinTestReadCloser{Reader: strings.NewReader(`{"path":"/native/claude","args":["claude"],"dir":"/definitely/missing"}`)}, gate: &darwinTestReadCloser{Reader: strings.NewReader("\x01")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			darwinLaunchExec = test.exec
			if darwinLaunchExec == nil {
				darwinLaunchExec = func(string, []string, []string) error { return nil }
			}
			if err := runDarwinLaunchBootstrap(test.config, test.gate); err == nil {
				t.Fatal("bootstrap error = nil")
			}
		})
	}
}

func TestDarwinLaunchBootstrapDispatch(t *testing.T) {
	useDarwinTestSupervisorIdentity(t)
	originalInput := darwinLaunchInput
	originalExit := darwinLaunchExit
	originalExec := darwinLaunchExec
	t.Cleanup(func() {
		darwinLaunchInput = originalInput
		darwinLaunchExit = originalExit
		darwinLaunchExec = originalExec
	})
	t.Setenv(darwinLaunchBootstrapEnv, darwinLaunchBootstrapMode)
	darwinLaunchExec = func(string, []string, []string) error { return nil }
	var exits []int
	darwinLaunchExit = func(code int) { exits = append(exits, code) }
	failureStatus := &darwinTestWriteCloser{}
	darwinLaunchInput = func() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
		return nil, nil, failureStatus, errors.New("input")
	}
	darwinLaunchBootstrap()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(darwinLaunchConfig{Path: "/native/claude", Args: []string{"claude"}, Dir: workingDirectory})
	if err != nil {
		t.Fatal(err)
	}
	status := &darwinTestWriteCloser{}
	darwinLaunchInput = func() (io.ReadCloser, io.ReadCloser, io.WriteCloser, error) {
		return io.NopCloser(bytes.NewReader(config)), io.NopCloser(strings.NewReader("\x01")), status, nil
	}
	darwinLaunchBootstrap()
	if len(exits) != 2 || exits[0] != 1 || exits[1] != 0 {
		t.Fatalf("exit codes = %v", exits)
	}
	if !status.closed || status.Len() != 0 {
		t.Fatalf("successful status = %q, closed=%v", status.String(), status.closed)
	}
	if !failureStatus.closed || failureStatus.String() != "input" {
		t.Fatalf("failure status = %q, closed=%v", failureStatus.String(), failureStatus.closed)
	}
}

func TestInheritedDarwinLaunchInputAndBoundedStatus(t *testing.T) {
	originalOpen := darwinLaunchOpenFile
	originalCloseExec := darwinLaunchCloseExec
	t.Cleanup(func() {
		darwinLaunchOpenFile = originalOpen
		darwinLaunchCloseExec = originalCloseExec
	})

	files := make([]*os.File, 3)
	for index := range files {
		file, err := os.CreateTemp(t.TempDir(), "descriptor")
		if err != nil {
			t.Fatal(err)
		}
		files[index] = file
	}
	index := 0
	darwinLaunchOpenFile = func(uintptr, string) *os.File {
		file := files[index]
		index++

		return file
	}
	closeExecFD := -1
	darwinLaunchCloseExec = func(fd int) error {
		closeExecFD = fd

		return nil
	}
	config, gate, status, err := inheritedDarwinLaunchInput()
	if err != nil || config == nil || gate == nil || status == nil || closeExecFD != int(files[2].Fd()) {
		t.Fatalf("inherited input = (%v,%v,%v), close-on-exec=%d, err=%v", config, gate, status, closeExecFD, err)
	}
	_ = config.Close()
	_ = gate.Close()
	_ = status.Close()

	index = 0
	darwinLaunchOpenFile = func(uintptr, string) *os.File {
		index++
		if index == 3 {
			return nil
		}

		return files[index-1]
	}
	if _, _, _, missingErr := inheritedDarwinLaunchInput(); missingErr == nil {
		t.Fatal("missing status descriptor was accepted")
	}

	closeExecFiles := make([]*os.File, 3)
	for index := range closeExecFiles {
		file, createErr := os.CreateTemp(t.TempDir(), "descriptor")
		if createErr != nil {
			t.Fatal(createErr)
		}
		closeExecFiles[index] = file
	}
	index = 0
	darwinLaunchOpenFile = func(uintptr, string) *os.File {
		file := closeExecFiles[index]
		index++

		return file
	}
	closeExecErr := errors.New("close-on-exec")
	darwinLaunchCloseExec = func(int) error { return closeExecErr }
	config, gate, status, err = inheritedDarwinLaunchInput()
	if !errors.Is(err, closeExecErr) {
		t.Fatalf("close-on-exec error = %v", err)
	}
	for _, file := range []io.Closer{config, gate, status} {
		_ = file.Close()
	}

	writer := &darwinTestWriteCloser{}
	reportDarwinLaunchStatus(writer, errors.New(strings.Repeat("x", darwinLaunchStatusLimit+100)))
	if !writer.closed || writer.Len() != darwinLaunchStatusLimit {
		t.Fatalf("bounded status length=%d closed=%v", writer.Len(), writer.closed)
	}
	reportDarwinLaunchStatus(nil, errors.New("ignored"))
}

func TestDarwinLaunchCloseOnExecChecked(t *testing.T) {
	original := darwinLaunchFcntl
	t.Cleanup(func() { darwinLaunchFcntl = original })

	calls := 0
	darwinLaunchFcntl = func(_ uintptr, command int, argument int) (int, error) {
		calls++
		if calls == 1 {
			if command != unix.F_GETFD || argument != 0 {
				t.Fatalf("get flags call = (%d,%d)", command, argument)
			}

			return 0, nil
		}
		if command != unix.F_SETFD || argument&unix.FD_CLOEXEC == 0 {
			t.Fatalf("set flags call = (%d,%d)", command, argument)
		}

		return 0, nil
	}
	if err := setDarwinLaunchCloseOnExec(5); err != nil || calls != 2 {
		t.Fatalf("checked close-on-exec calls=%d err=%v", calls, err)
	}

	want := errors.New("fcntl")
	darwinLaunchFcntl = func(uintptr, int, int) (int, error) { return 0, want }
	if err := setDarwinLaunchCloseOnExec(5); !errors.Is(err, want) {
		t.Fatalf("get flags error = %v", err)
	}
	calls = 0
	darwinLaunchFcntl = func(uintptr, int, int) (int, error) {
		calls++
		if calls == 1 {
			return 0, nil
		}

		return 0, want
	}
	if err := setDarwinLaunchCloseOnExec(5); !errors.Is(err, want) {
		t.Fatalf("set flags error = %v", err)
	}
}

func TestAwaitDarwinNativeExecStatus(t *testing.T) {
	originalTimeout := darwinLaunchTimeout
	t.Cleanup(func() { darwinLaunchTimeout = originalTimeout })

	t.Run("success eof", func(t *testing.T) {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = write.Close()
		launch := &processTreeCommand{ready: read}
		if err := awaitProcessTreeReady(launch); err != nil || launch.ready != nil {
			t.Fatalf("await success error=%v ready=%v", err, launch.ready)
		}
	})

	t.Run("failure payload", func(t *testing.T) {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(write, "exec native Claude command: missing")
		_ = write.Close()
		err = awaitProcessTreeReady(&processTreeCommand{ready: read})
		if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("await payload error = %v", err)
		}
	})

	t.Run("oversized payload", func(t *testing.T) {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(write, strings.Repeat("x", darwinLaunchStatusLimit+1))
		_ = write.Close()
		err = awaitProcessTreeReady(&processTreeCommand{ready: read})
		if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("oversized status error = %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer write.Close()
		darwinLaunchTimeout = 10 * time.Millisecond
		err = awaitProcessTreeReady(&processTreeCommand{ready: read})
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("timeout error = %v", err)
		}
	})

	t.Run("deadline unsupported", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "regular")
		if err != nil {
			t.Fatal(err)
		}
		err = awaitProcessTreeReady(&processTreeCommand{ready: file})
		if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("regular-file ready error = %v", err)
		}
	})

	if err := awaitProcessTreeReady(nil); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinMissingNativeExecutableFailsBeforeLaunchAdmission(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	generation := &DarwinGeneration{RuntimeID: strings.Repeat("a", 32), ScratchRoot: t.TempDir()}
	launch, err := prepareProcessTreeCommand(exec.Command(filepath.Join(t.TempDir(), "missing-native")), processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       generation,
		Isolation:        testProcessIsolation(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = startContainedProcess(launch)
	if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "exec native Claude command") {
		t.Fatalf("missing native launch error = %v", err)
	}
}

func TestDarwinBootstrapEnvironmentIsPrivate(t *testing.T) {
	privateKey := privateAdapterEnvPrefix + "SECRET"
	t.Setenv(privateKey, "must-not-pass")
	t.Setenv(DarwinRuntimeIDEnv, strings.Repeat("b", 32))
	t.Setenv(DarwinScratchRootEnv, "/private/root")
	t.Setenv("GORACE", "halt_on_error=1")

	originalUID, originalGID := processEffectiveUID, processEffectiveGID
	processEffectiveUID = func() int { return 1 }
	processEffectiveGID = func() int { return 2 }

	t.Cleanup(func() { processEffectiveUID, processEffectiveGID = originalUID, originalGID })

	env := testEnvironmentMap(ordinaryLaunchIdentityEnvironment(
		[]string{"BASE=yes"}, darwinLaunchBootstrapEnv, darwinLaunchBootstrapMode,
	))
	if env[darwinLaunchBootstrapEnv] != darwinLaunchBootstrapMode {
		t.Fatalf("bootstrap environment = %#v", env)
	}
	if env["BASE"] != "yes" || env[processIsolationUIDEnv] != "1" || env[processIsolationGIDEnv] != "2" {
		t.Fatalf("bootstrap policy environment = %#v", env)
	}
	for _, key := range []string{privateKey, DarwinRuntimeIDEnv, DarwinScratchRootEnv, "GORACE"} {
		if _, ok := env[key]; ok {
			t.Fatalf("private environment leaked %s: %#v", key, env)
		}
	}
}

func TestPrepareDarwinLaunchResourceFailures(t *testing.T) {
	originalCreateTemp := darwinLaunchCreateTemp
	originalRemove := darwinLaunchRemove
	originalPipe := darwinLaunchPipe
	originalEncode := darwinLaunchEncode
	originalExecutable := darwinLaunchExecutable
	originalCommand := darwinLaunchCommand
	t.Cleanup(func() {
		darwinLaunchCreateTemp = originalCreateTemp
		darwinLaunchRemove = originalRemove
		darwinLaunchPipe = originalPipe
		darwinLaunchEncode = originalEncode
		darwinLaunchExecutable = originalExecutable
		darwinLaunchCommand = originalCommand
	})

	options := processLaunchOptions{DarwinBestEffort: true, Generation: &DarwinGeneration{ScratchRoot: t.TempDir()}}
	if _, err := prepareProcessTreeCommand(exec.Command("true"), processLaunchOptions{DarwinBestEffort: true}); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("missing generation error = %v", err)
	}
	if _, err := prepareProcessTreeCommand(&exec.Cmd{}, options); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete command error = %v", err)
	}
	fileRoot := filepath.Join(t.TempDir(), "generation-file")
	if err := os.WriteFile(fileRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProcessTreeCommand(exec.Command("true"), processLaunchOptions{
		DarwinBestEffort: true,
		Generation:       &DarwinGeneration{ScratchRoot: fileRoot},
	}); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("generation preparation error = %v", err)
	}

	want := errors.New("resource")
	darwinLaunchCreateTemp = func(string, string) (*os.File, error) { return nil, want }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) {
		t.Fatalf("create config error = %v", err)
	}
	darwinLaunchCreateTemp = func(dir, pattern string) (*os.File, error) {
		file, err := os.CreateTemp(dir, pattern)
		if err == nil {
			_ = file.Close()
		}

		return file, err
	}
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); err == nil || !strings.Contains(err.Error(), "secure") {
		t.Fatalf("chmod config error = %v", err)
	}
	darwinLaunchCreateTemp = originalCreateTemp

	darwinLaunchEncode = func(io.Writer, any) error { return want }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) {
		t.Fatalf("encode config error = %v", err)
	}
	darwinLaunchEncode = func(output io.Writer, value any) error {
		file, ok := output.(*os.File)
		if !ok {
			return errors.New("launch config output is not a file")
		}

		if err := json.NewEncoder(file).Encode(value); err != nil {
			return err
		}

		return file.Close()
	}
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); err == nil || !strings.Contains(err.Error(), "rewind") {
		t.Fatalf("seek config error = %v", err)
	}
	darwinLaunchEncode = originalEncode

	darwinLaunchRemove = func(string) error { return want }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) {
		t.Fatalf("unlink config error = %v", err)
	}
	darwinLaunchRemove = originalRemove

	darwinLaunchPipe = func() (*os.File, *os.File, error) { return nil, nil, want }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("gate pipe error = %v", err)
	}
	pipeCalls := 0
	darwinLaunchPipe = func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 2 {
			return nil, nil, want
		}

		return os.Pipe()
	}
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) || !strings.Contains(err.Error(), "status") {
		t.Fatalf("status pipe error = %v", err)
	}
	darwinLaunchPipe = originalPipe

	darwinLaunchExecutable = func() (string, error) { return "", want }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); !errors.Is(err, want) || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("executable error = %v", err)
	}
	darwinLaunchExecutable = func() (string, error) { return "relative-bootstrap", nil }
	if _, err := prepareProcessTreeCommand(exec.Command("true"), options); err == nil || !strings.Contains(err.Error(), "through process policy") {
		t.Fatalf("bootstrap resolution error = %v", err)
	}
	darwinLaunchExecutable = originalExecutable

	// The best-effort helper runs as the identity the adapter already holds, so
	// it never carries a credential change to apply.
	launch, err := prepareProcessTreeCommand(exec.Command("true"), options)
	if err != nil {
		t.Fatalf("best-effort launch error = %v", err)
	}
	if launch.cmd.SysProcAttr == nil || !launch.cmd.SysProcAttr.Setpgid || launch.cmd.SysProcAttr.Credential != nil {
		t.Fatalf("best-effort helper attributes = %#v", launch.cmd.SysProcAttr)
	}
	launch.close()
}
