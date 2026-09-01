package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type residualWriteCloser struct {
	write func([]byte) (int, error)
	close func() error
}

func (w residualWriteCloser) Write(data []byte) (int, error) {
	if w.write != nil {
		return w.write(data)
	}

	return len(data), nil
}

func (w residualWriteCloser) Close() error {
	if w.close != nil {
		return w.close()
	}

	return nil
}

type residualReadCloser struct {
	read  func([]byte) (int, error)
	close func() error
}

func (r residualReadCloser) Read(data []byte) (int, error) {
	if r.read != nil {
		return r.read(data)
	}

	return 0, io.EOF
}

func (r residualReadCloser) Close() error {
	if r.close != nil {
		return r.close()
	}

	return nil
}

func TestNativeProcessResidualBranches(t *testing.T) {
	_, err := startNative(t.Context(), Options{Authority: &NativeAuthority{}}, "claude", nil)
	require.Error(t, err)

	_, err = startNative(t.Context(), Options{
		Authority: &NativeAuthority{NativeEnvironment: func() map[string]string { return nil }},
	}, "claude", nil)
	require.Error(t, err)

	_, err = startNative(t.Context(), Options{OrdinaryEnvironment: map[string]string{"BAD": "nul\x00"}}, "claude", nil)
	require.ErrorContains(t, err, "environment")
	require.ErrorContains(t, authorityUnavailable(nil), "unavailable")

	previousDirectory, err := os.Getwd()
	require.NoError(t, err)
	deletedDirectory := t.TempDir()
	require.NoError(t, os.Chdir(deletedDirectory))
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })
	require.NoError(t, os.Remove(deletedDirectory))
	_, err = startNative(t.Context(), Options{PreparedEnvironment: []string{"PATH=/bin"}}, "/bin/true", nil)
	require.ErrorContains(t, err, "working directory")
	require.NoError(t, os.Chdir(previousDirectory))
}

func TestOrdinaryProcessResidualBranches(t *testing.T) {
	previous := ordinaryGetwd
	ordinaryGetwd = func() (string, error) { return "", errors.New("getwd refused") }
	t.Cleanup(func() { ordinaryGetwd = previous })

	_, err := resolveOrdinaryExecutable("relative/path", nil)
	require.ErrorContains(t, err, "getwd refused")
	_, err = resolveOrdinaryExecutable("claude", []string{"PATH=relative"})
	require.ErrorContains(t, err, "getwd refused")
	ordinaryGetwd = previous
	resolved, err := resolveOrdinaryExecutable("true", []string{"PATH=:/bin"})
	require.NoError(t, err)
	require.Equal(t, "/bin/true", resolved)

	notExecutable := filepath.Join(t.TempDir(), "not-an-executable")
	require.NoError(t, os.WriteFile(notExecutable, []byte("not an executable format"), 0o700))
	_, err = startOrdinaryNative(notExecutable, nil, []string{"PATH=/bin"}, t.TempDir())
	require.ErrorContains(t, err, "start native process")

	done := make(chan struct{})
	close(done)
	process := &ordinaryProcess{waitDone: done, revokeDone: make(chan struct{}), stdin: residualWriteCloser{}}
	require.NoError(t, process.Revoke(t.Context()))

	process = &ordinaryProcess{waitDone: make(chan struct{}), revokeDone: make(chan struct{}), stdin: residualWriteCloser{}}
	process.collectOnce.Do(func() {})
	_, err = process.Wait(residualCancelledContext())
	require.ErrorIs(t, err, context.Canceled)

	invalid, findErr := os.FindProcess(-1)
	require.NoError(t, findErr)
	process = &ordinaryProcess{
		command:    &exec.Cmd{Process: invalid},
		stdin:      residualWriteCloser{},
		waitDone:   make(chan struct{}),
		revokeDone: make(chan struct{}),
	}
	process.collectOnce.Do(func() {})
	require.Error(t, process.Revoke(t.Context()))

	command := exec.Command("/bin/true")
	require.NoError(t, command.Start())
	process = &ordinaryProcess{command: command, waitDone: make(chan struct{}), revokeDone: make(chan struct{})}
	_, err = process.Wait(t.Context())
	require.NoError(t, err)

	command = exec.Command("/bin/sh", "-c", "sleep 60")
	require.NoError(t, command.Start())
	process = &ordinaryProcess{
		command:    command,
		stdin:      residualWriteCloser{},
		waitDone:   make(chan struct{}),
		revokeDone: make(chan struct{}),
	}
	require.NoError(t, process.Revoke(t.Context()))
	_, err = process.Wait(t.Context())
	require.NoError(t, err)
}

func TestOrdinaryProcessPipeFailureCleanup(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			calls := 0
			openPipe := func() (*os.File, *os.File, error) {
				calls++
				if calls == failAt {
					return nil, nil, errors.New("pipe refused")
				}

				return os.Pipe()
			}
			_, err := startOrdinaryNativeWithPipe("/bin/true", nil, []string{"PATH=/bin"}, t.TempDir(), openPipe)
			require.ErrorContains(t, err, "pipe refused")
		})
	}
}

func TestRunNativeOutputResidualBranches(t *testing.T) {
	baseAuthority := func() *NativeAuthority {
		return &NativeAuthority{
			Unavailable:           errors.New("unavailable"),
			ContainmentIncomplete: errors.New("incomplete"),
			NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/bin"} },
		}
	}

	authority := baseAuthority()
	_, _, err := runNativeOutput(t.Context(), Options{Authority: authority, ClaudeHome: "/home"}, "claude", nil)
	require.ErrorIs(t, err, authority.Unavailable)

	for _, sentinel := range []bool{true, false} {
		authority = baseAuthority()
		prepareErr := errors.New("prepare refused")
		if sentinel {
			prepareErr = authority.ContainmentIncomplete
		}
		authority.PrepareNativeTree = func(context.Context, string) error { return prepareErr }
		_, _, err = runNativeOutput(t.Context(), Options{Authority: authority, ClaudeHome: "/home"}, "claude", nil)
		if sentinel {
			require.ErrorIs(t, err, authority.ContainmentIncomplete)
		} else {
			require.ErrorIs(t, err, authority.ContainmentIncomplete)
			require.ErrorContains(t, err, "prepare native output tree")
		}
	}

	authority = baseAuthority()
	authority.PrepareNativeTree = func(context.Context, string) error { return nil }
	authority.ReclaimNativeTree = func(context.Context, string) error { return errors.New("reclaim refused") }
	authority.StartNative = func(context.Context, NativeRequest) (NativeProcess, error) {
		return &authorityTestProcess{
			stdin:  residualWriteCloser{},
			stdout: io.NopCloser(bytes.NewReader(nil)),
			stderr: io.NopCloser(bytes.NewReader(nil)),
			wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
			revoke: func(context.Context) error { return nil },
		}, nil
	}
	_, _, err = runNativeOutput(t.Context(), Options{Authority: authority, ClaudeHome: "/home"}, "claude", nil)
	require.ErrorContains(t, err, "reclaim refused")

	previousShutdown := processShutdownWaitDelay
	processShutdownWaitDelay = 2 * time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = previousShutdown })

	t.Run("terminal authority wait error", func(t *testing.T) {
		release := make(chan struct{})
		authority := baseAuthority()
		authority.StartNative = func(context.Context, NativeRequest) (NativeProcess, error) {
			return &authorityTestProcess{
				stdin:  residualWriteCloser{},
				stdout: io.NopCloser(bytes.NewReader(nil)),
				stderr: io.NopCloser(bytes.NewReader(nil)),
				wait: func(context.Context) (NativeResult, error) {
					<-release

					return NativeResult{}, errors.New("wait refused")
				},
				revoke: func(context.Context) error {
					close(release)

					return nil
				},
			}, nil
		}
		_, _, err := runNativeOutput(residualCancelledContext(), Options{Authority: authority, TreePrepared: true}, "claude", nil)
		require.ErrorIs(t, err, authority.ContainmentIncomplete)
	})

	t.Run("terminal wait timeout", func(t *testing.T) {
		release := make(chan struct{})
		authority := baseAuthority()
		authority.StartNative = func(context.Context, NativeRequest) (NativeProcess, error) {
			return &authorityTestProcess{
				stdin: residualWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
				wait: func(context.Context) (NativeResult, error) {
					<-release

					return NativeResult{}, nil
				},
				revoke: func(context.Context) error { return nil },
			}, nil
		}
		_, _, err := runNativeOutput(residualCancelledContext(), Options{Authority: authority, TreePrepared: true}, "claude", nil)
		require.ErrorIs(t, err, authority.ContainmentIncomplete)
		close(release)
	})

	waitErr := errors.New("caller canceled")
	terminalErr := errors.New("wait refused")
	classified, terminal := classifyNativeOutputWait(Options{}, waitErr, terminalErr)
	require.ErrorIs(t, classified, waitErr)
	require.ErrorIs(t, classified, terminalErr)
	require.False(t, terminal)
}

func TestAuthStatusAndGrammarResidualBranches(t *testing.T) {
	options := Options{Authority: &NativeAuthority{NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} }}, TreePrepared: true}
	_, _, err := AuthStatus(t.Context(), options)
	require.Error(t, err)
	_, _, err = authCommandOutput(t.Context(), nil, options)
	require.Error(t, err)

	require.True(t, authURLTerminator(' '))
	require.True(t, authURLTerminator('\n'))
	require.False(t, authURLTerminator('a'))

	second := "https://claude.com/oauth/authorize?code=2&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback"
	_, err = classifyAuthLoginLine(currentAuthorizeURL + " " + second)
	require.ErrorIs(t, err, ErrAuthLoginGrammar)
}

func TestStartAuthLoginResidualFailures(t *testing.T) {
	scratchFile := filepath.Join(t.TempDir(), "scratch-file")
	require.NoError(t, os.WriteFile(scratchFile, []byte("x"), 0o600))
	_, _, err := StartAuthLogin(t.Context(), Options{ScratchParent: scratchFile})
	require.ErrorContains(t, err, "browser launch")

	_, err = startAuthLoginChild(Options{
		ScratchParent: t.TempDir(),
		Authority: &NativeAuthority{
			NativeEnvironment: func() map[string]string { return nil },
		},
	})
	require.Error(t, err)

	_, err = startAuthLoginChild(Options{
		ScratchParent: t.TempDir(),
		Authority: &NativeAuthority{
			NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		},
	})
	require.Error(t, err)

	incomplete := errors.New("incomplete")
	_, err = startAuthLoginChild(Options{
		ClaudeHome:    "",
		ScratchParent: t.TempDir(),
		Authority: &NativeAuthority{
			NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/bin"} },
			ContainmentIncomplete: incomplete,
			PrepareNativeTree:     func(context.Context, string) error { return incomplete },
		},
	})
	require.ErrorIs(t, err, incomplete)

	authority := &NativeAuthority{
		Unavailable:           errors.New("unavailable"),
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/bin"} },
		PrepareNativeTree:     func(context.Context, string) error { return nil },
		ReclaimNativeTree:     func(context.Context, string) error { return nil },
		StartNative:           func(context.Context, NativeRequest) (NativeProcess, error) { return nil, incomplete },
	}
	_, err = startAuthLoginChild(Options{ScratchParent: t.TempDir(), Authority: authority, TreePrepared: true})
	require.ErrorIs(t, err, incomplete)
}

func TestStartAuthLoginPresentationTimeout(t *testing.T) {
	previousReader := authLoginPresentationReader
	previousWait := authLoginPresentationWait
	releaseReader := make(chan struct{})
	readerDone := make(chan struct{})
	authLoginPresentationReader = func(io.Reader) (string, error) {
		defer close(readerDone)
		<-releaseReader

		return "", io.EOF
	}
	authLoginPresentationWait = time.Millisecond
	defer func() {
		authLoginPresentationReader = previousReader
		authLoginPresentationWait = previousWait
	}()

	revoked := make(chan struct{})
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		PrepareNativeTree: func(context.Context, string) error { return nil },
		ReclaimNativeTree: func(context.Context, string) error { return nil },
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			return &authorityTestProcess{
				stdin: residualWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
				wait: func(ctx context.Context) (NativeResult, error) {
					select {
					case <-revoked:
						return NativeResult{}, nil
					case <-ctx.Done():
						return NativeResult{}, ctx.Err()
					}
				},
				revoke: func(context.Context) error {
					close(revoked)

					return nil
				},
			}, nil
		},
	}
	_, _, err := StartAuthLogin(t.Context(), Options{ScratchParent: t.TempDir(), Authority: authority, TreePrepared: true})
	close(releaseReader)
	<-readerDone
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAuthLoginStateResidualBranches(t *testing.T) {
	closeErr := errors.New("close refused")
	login := &AuthLogin{stdin: residualWriteCloser{
		write: func([]byte) (int, error) { return 0, errors.New("write refused") },
		close: func() error { return closeErr },
	}}
	require.ErrorIs(t, login.Submit("value"), closeErr)

	done := make(chan struct{})
	close(done)
	login = &AuthLogin{exitDone: done}
	require.True(t, login.Exited())

	login = &AuthLogin{exitDone: make(chan struct{})}
	exit, err := login.Wait(residualCancelledContext())
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, AuthLoginExitUnknown, exit)

	login = &AuthLogin{exitDone: done, exitErr: errors.New("wait refused")}
	exit, err = login.Wait(t.Context())
	require.ErrorContains(t, err, "wait refused")
	require.Equal(t, AuthLoginExitUnknown, exit)

	login = &AuthLogin{exitDone: done, exitResult: NativeResult{ExitCode: 3}}
	exit, err = login.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, AuthLoginExitNonzero, exit)

	login = &AuthLogin{processSettled: true, shimRemoved: true}
	require.False(t, login.CleanupPending())
	login.prepared = []string{"root"}
	require.True(t, login.CleanupPending())
}

func TestAuthLoginCleanupResidualBranches(t *testing.T) {
	require.NoError(t, reclaimNativeTree(nil, "root"))
	require.NoError(t, reclaimNativeTree(&NativeAuthority{}, ""))
	require.Error(t, reclaimNativeTree(&NativeAuthority{}, "root"))

	busy := errors.New("busy")
	incomplete := errors.New("incomplete")
	authority := &NativeAuthority{
		TreeBusy:              busy,
		ContainmentIncomplete: incomplete,
		ReclaimNativeTree:     func(context.Context, string) error { return busy },
	}
	require.ErrorIs(t, reclaimNativeTree(authority, "root"), busy)
	authority.ReclaimNativeTree = func(context.Context, string) error { return errors.New("reclaim refused") }
	require.ErrorIs(t, reclaimNativeTree(authority, "root"), incomplete)
	authority.ContainmentIncomplete = nil
	require.ErrorContains(t, reclaimNativeTree(authority, "root"), "reclaim refused")

	done := make(chan struct{})
	close(done)
	login := &AuthLogin{exitDone: done, exitErr: errors.New("exit refused")}
	require.ErrorContains(t, login.reap(t.Context()), "exit refused")
	require.ErrorIs(t, (&AuthLogin{exitDone: make(chan struct{})}).reap(residualCancelledContext()), context.Canceled)

	login = &AuthLogin{
		processSettled: true,
		prepared:       []string{"root"},
		shim:           &browserShim{dir: "\x00"},
		options: Options{Authority: &NativeAuthority{
			ReclaimNativeTree: func(context.Context, string) error { return errors.New("reclaim refused") },
		}},
	}
	require.ErrorContains(t, login.Close(), "reclaim refused")
	login.prepared = nil
	require.Error(t, login.Close())
}

func TestAuthLoginCloseSettlementResidualBranches(t *testing.T) {
	done := make(chan struct{})
	close(done)
	newLogin := func(exitErr, revokeErr, stdoutErr error, authority *NativeAuthority) *AuthLogin {
		return &AuthLogin{
			stdin:    residualWriteCloser{},
			stdout:   residualReadCloser{close: func() error { return stdoutErr }},
			exitDone: done,
			exitErr:  exitErr,
			process: &authorityTestProcess{
				revoke: func(context.Context) error { return revokeErr },
			},
			shim:    &browserShim{},
			options: Options{Authority: authority},
		}
	}

	login := newLogin(nil, context.Canceled, os.ErrClosed, nil)
	require.NoError(t, login.Close())
	require.False(t, login.CleanupPending())

	incomplete := errors.New("incomplete")
	authority := &NativeAuthority{ContainmentIncomplete: incomplete}
	login = newLogin(context.Canceled, nil, nil, authority)
	require.ErrorIs(t, login.Close(), incomplete)

	login = newLogin(errors.New("wait refused"), nil, nil, authority)
	require.ErrorIs(t, login.Close(), incomplete)
}

func TestNativeOutputBufferAndDrainResidualBranches(t *testing.T) {
	var bounded boundedNativeOutput
	data := bytes.Repeat([]byte("x"), nativeOutputMaxBytes+1)
	written, err := bounded.Write(data)
	require.NoError(t, err)
	require.Len(t, data, written)
	require.Error(t, bounded.err)
	written, err = bounded.Write([]byte("more"))
	require.NoError(t, err)
	require.Equal(t, 4, written)

	previous := nativeOutputDrainDelay
	nativeOutputDrainDelay = time.Millisecond
	t.Cleanup(func() { nativeOutputDrainDelay = previous })

	closeErr := errors.New("close refused")
	stdoutDone := make(chan nativeOutputRead, 1)
	stdoutDone <- nativeOutputRead{data: []byte("done")}
	read := awaitNativeOutput(stdoutDone, residualReadCloser{})
	require.Equal(t, []byte("done"), read.data)

	read = awaitNativeOutput(make(chan nativeOutputRead), residualReadCloser{close: func() error { return closeErr }})
	require.ErrorIs(t, read.err, closeErr)

	stderrDone := make(chan error, 1)
	stderrDone <- errors.New("drain refused")
	require.ErrorContains(t, awaitNativeDrain(stderrDone, residualReadCloser{}), "drain refused")
	require.ErrorIs(t, awaitNativeDrain(make(chan error), residualReadCloser{close: func() error { return closeErr }}), closeErr)
}

func TestProcessTransportResidualBranches(t *testing.T) {
	previousProbe := claudeVersionProbe
	claudeVersionProbe = func(context.Context, Options) error { return errors.New("probe refused") }
	t.Cleanup(func() { claudeVersionProbe = previousProbe })
	require.ErrorContains(t, NewProcessTransport(nil, Options{}).Start(t.Context()), "probe refused")

	claudeVersionProbe = func(context.Context, Options) error { return nil }
	transport := NewProcessTransport(nil, Options{Authority: &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
	}})
	require.ErrorContains(t, transport.Start(t.Context()), "start claude")

	ctx, cancel := context.WithCancel(t.Context())
	transport = &ProcessTransport{stdin: residualWriteCloser{write: func([]byte) (int, error) {
		cancel()

		return 0, errors.New("write refused")
	}}}
	require.ErrorIs(t, transport.Send(ctx, map[string]any{}), context.Canceled)

	transport = &ProcessTransport{stdin: residualWriteCloser{close: func() error { return os.ErrClosed }}}
	require.NoError(t, transport.closeStdin())
	require.NoError(t, transport.closeStdin())
	require.NoError(t, closeNativeStream(residualReadCloser{close: func() error { return os.ErrClosed }}))

	deadlineWriter := &transportDeadlineWriter{}
	reset := installWriteDeadline(t.Context(), deadlineWriter)
	reset()
	require.Len(t, deadlineWriter.deadlines, 1)

	deadlineCtx, deadlineCancel := context.WithCancel(t.Context())
	reset = installWriteDeadline(deadlineCtx, deadlineWriter)
	deadlineCancel()
	require.Eventually(t, func() bool {
		deadlineWriter.mu.Lock()
		defer deadlineWriter.mu.Unlock()

		return len(deadlineWriter.deadlines) >= 2
	}, time.Second, time.Millisecond)
	reset()

	result, err := (&ProcessTransport{}).wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, NativeResult{}, result)
}

func TestProcessTransportEventAndCloseResidualBranches(t *testing.T) {
	previousDrain := nativeOutputDrainDelay
	previousGrace := processExitGracePeriod
	previousShutdown := processShutdownWaitDelay
	nativeOutputDrainDelay = time.Millisecond
	processExitGracePeriod = time.Millisecond
	processShutdownWaitDelay = time.Millisecond
	t.Cleanup(func() {
		nativeOutputDrainDelay = previousDrain
		processExitGracePeriod = previousGrace
		processShutdownWaitDelay = previousShutdown
	})

	t.Run("stderr misses both drains", func(t *testing.T) {
		transport := &ProcessTransport{
			stdout: io.NopCloser(bytes.NewReader(nil)),
			stderr: residualReadCloser{read: func([]byte) (int, error) {
				time.Sleep(10 * time.Millisecond)

				return 0, io.EOF
			}},
		}
		_, errs := splitEventsForTest(transport.Events(t.Context()))
		require.ErrorIs(t, <-errs, errClaudeTransportFailure)
	})

	t.Run("canceled delivery", func(t *testing.T) {
		transport := &ProcessTransport{stdout: io.NopCloser(bytes.NewBufferString("{\"type\":\"result\"}\n"))}
		events := make(chan TransportEvent)
		_, deliver := transport.scanEvents(residualCancelledContext(), events)
		require.False(t, deliver)

		for range transport.Events(residualCancelledContext()) {
		}
	})

	t.Run("terminal wait timeout", func(t *testing.T) {
		release := make(chan struct{})
		process := &authorityTestProcess{
			wait: func(context.Context) (NativeResult, error) {
				<-release

				return NativeResult{}, nil
			},
			revoke: func(context.Context) error { return nil },
		}
		transport := &ProcessTransport{
			process: process,
			options: Options{Authority: &NativeAuthority{ContainmentIncomplete: errors.New("incomplete")}},
		}
		require.ErrorIs(t, transport.Close(), transport.options.Authority.ContainmentIncomplete)
		close(release)
	})

	t.Run("authority wait error", func(t *testing.T) {
		incomplete := errors.New("incomplete")
		transport := &ProcessTransport{
			process: &authorityTestProcess{
				wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, errors.New("wait refused") },
				revoke: func(context.Context) error { return nil },
			},
			options: Options{Authority: &NativeAuthority{ContainmentIncomplete: incomplete}},
		}
		require.ErrorIs(t, transport.Close(), incomplete)
	})

	t.Run("stream cleanup timeouts", func(t *testing.T) {
		transport := &ProcessTransport{
			eventsDone: make(chan struct{}),
			stdout:     residualReadCloser{close: func() error { return errors.New("stdout close refused") }},
			stderr:     residualReadCloser{},
			stderrDone: make(chan struct{}),
		}
		err := transport.Close()
		require.ErrorContains(t, err, "stdout close refused")
		require.ErrorIs(t, err, errClaudeTransportFailure)
	})
}

func residualCancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}
