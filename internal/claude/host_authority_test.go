package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type authorityTestWriteCloser struct{ bytes.Buffer }

func (*authorityTestWriteCloser) Close() error { return nil }

type authorityTestCloseFunc func() error

func (authorityTestCloseFunc) Write(data []byte) (int, error) { return len(data), nil }
func (f authorityTestCloseFunc) Close() error                 { return f() }

type authorityTestProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	wait   func(context.Context) (NativeResult, error)
	revoke func(context.Context) error
}

func (p *authorityTestProcess) Stdin() io.WriteCloser                          { return p.stdin }
func (p *authorityTestProcess) Stdout() io.ReadCloser                          { return p.stdout }
func (p *authorityTestProcess) Stderr() io.ReadCloser                          { return p.stderr }
func (p *authorityTestProcess) Wait(ctx context.Context) (NativeResult, error) { return p.wait(ctx) }
func (p *authorityTestProcess) Revoke(ctx context.Context) error               { return p.revoke(ctx) }

func TestProcessTransportRoutesProbeAndSessionThroughAuthorityAndWaitsAfterEOF(t *testing.T) {
	var mu sync.Mutex
	requests := []NativeRequest{}
	waitRelease := make(chan struct{})
	starts := 0
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin", "BASE": "host"} },
		StartNative: func(_ context.Context, request NativeRequest) (NativeProcess, error) {
			mu.Lock()
			requests = append(requests, request)
			starts++
			current := starts
			mu.Unlock()
			output := "2.1.0\n"
			wait := func(context.Context) (NativeResult, error) { return NativeResult{}, nil }
			if current == 2 {
				output = "{\"type\":\"result\"}\n"
				wait = func(ctx context.Context) (NativeResult, error) {
					select {
					case <-waitRelease:
						return NativeResult{}, nil
					case <-ctx.Done():
						return NativeResult{}, ctx.Err()
					}
				}
			}

			return &authorityTestProcess{stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewBufferString(output)), stderr: io.NopCloser(bytes.NewReader(nil)), wait: wait, revoke: func(context.Context) error { return nil }}, nil
		},
	}
	transport := NewProcessTransport(nil, Options{CLIPath: "claude-logical", Cwd: "/workspace", ClaudeHome: "/native/home", Env: map[string]string{"OVERLAY": "yes"}, Authority: authority, TreePrepared: true})
	require.NoError(t, transport.Start(t.Context()))
	events := transport.Events(t.Context())
	frame := <-events
	require.Equal(t, "result", frame.Message["type"])
	select {
	case <-events:
		t.Fatal("events closed before NativeProcess.Wait completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(waitRelease)
	_, ok := <-events
	require.False(t, ok)

	require.Len(t, requests, 2)
	require.Equal(t, []string{"--version"}, requests[0].Arguments)
	require.Equal(t, "claude-logical", requests[0].Executable)
	require.Equal(t, "/workspace", requests[1].WorkingDirectory)
	require.Contains(t, requests[1].Environment, "BASE=host")
	require.Contains(t, requests[1].Environment, "OVERLAY=yes")
}

func TestProcessTransportCloseUsesProtocolThenRevokeAndWait(t *testing.T) {
	priorGrace, priorShutdown := processExitGracePeriod, processShutdownWaitDelay
	processExitGracePeriod, processShutdownWaitDelay = time.Millisecond, time.Second
	t.Cleanup(func() { processExitGracePeriod, processShutdownWaitDelay = priorGrace, priorShutdown })

	revoked := make(chan struct{})
	var once sync.Once
	var revokeCalls atomic.Int32
	process := &authorityTestProcess{
		stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
		wait: func(ctx context.Context) (NativeResult, error) {
			select {
			case <-revoked:
				return NativeResult{Revoked: true}, nil
			case <-ctx.Done():
				return NativeResult{}, ctx.Err()
			}
		},
		revoke: func(context.Context) error {
			revokeCalls.Add(1)
			once.Do(func() { close(revoked) })

			return nil
		},
	}
	starts := 0
	authority := &NativeAuthority{NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} }, StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
		starts++
		if starts == 1 {
			return &authorityTestProcess{stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewBufferString("2.0.0\n")), stderr: io.NopCloser(bytes.NewReader(nil)), wait: func(context.Context) (NativeResult, error) { return NativeResult{}, nil }, revoke: func(context.Context) error { return nil }}, nil
		}

		return process, nil
	}}
	transport := NewProcessTransport(nil, Options{Authority: authority})
	require.NoError(t, transport.Start(t.Context()))
	var closes sync.WaitGroup
	for range 8 {
		closes.Add(1)
		go func() { defer closes.Done(); require.NoError(t, transport.Close()) }()
	}
	closes.Wait()
	select {
	case <-revoked:
	default:
		t.Fatal("Close did not revoke the live native process")
	}
	require.Equal(t, int32(1), revokeCalls.Load())
}

func TestProcessTransportCloseDeliversBufferedTerminalResultBeforeReturning(t *testing.T) {
	priorProbe := claudeVersionProbe
	claudeVersionProbe = func(context.Context, Options) error { return nil }
	t.Cleanup(func() { claudeVersionProbe = priorProbe })

	stdout, output := io.Pipe()
	terminal := make(chan struct{})
	var terminalOnce sync.Once
	var waitCalls atomic.Int32
	process := &authorityTestProcess{
		stdin:  nil,
		stdout: stdout,
		stderr: io.NopCloser(bytes.NewReader(nil)),
		wait: func(ctx context.Context) (NativeResult, error) {
			waitCalls.Add(1)
			select {
			case <-terminal:
				return NativeResult{}, nil
			case <-ctx.Done():
				return NativeResult{}, ctx.Err()
			}
		},
		revoke: func(context.Context) error { return errors.New("unexpected revoke") },
	}
	process.stdin = authorityTestCloseFunc(func() error {
		_, writeErr := io.WriteString(output, "{\"type\":\"result\",\"result\":\"done\"}\n")
		closeErr := output.Close()
		terminalOnce.Do(func() { close(terminal) })

		return errors.Join(writeErr, closeErr)
	})
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		StartNative:       func(context.Context, NativeRequest) (NativeProcess, error) { return process, nil },
	}
	transport := NewProcessTransport(nil, Options{CLIPath: "claude", Authority: authority})
	require.NoError(t, transport.Start(t.Context()))
	events := transport.Events(t.Context())

	closed := make(chan error, 1)
	go func() { closed <- transport.Close() }()

	event := <-events
	require.Equal(t, "result", event.Message["type"])
	require.Equal(t, "done", event.Message["result"])
	require.NoError(t, <-closed)
	require.Equal(t, int32(1), waitCalls.Load())
	_, ok := <-events
	require.False(t, ok)
}

func TestProcessTransportRevokeCallerTimeoutDoesNotOverrideEventualTerminalWait(t *testing.T) {
	priorGrace, priorShutdown := processExitGracePeriod, processShutdownWaitDelay
	processExitGracePeriod, processShutdownWaitDelay = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { processExitGracePeriod, processShutdownWaitDelay = priorGrace, priorShutdown })

	terminal := make(chan struct{})
	var revokeOnce sync.Once
	process := &authorityTestProcess{
		stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
		wait: func(ctx context.Context) (NativeResult, error) {
			select {
			case <-terminal:
				return NativeResult{Revoked: true}, nil
			case <-ctx.Done():
				return NativeResult{}, ctx.Err()
			}
		},
		revoke: func(ctx context.Context) error {
			revokeOnce.Do(func() {
				go func() {
					time.Sleep(75 * time.Millisecond)
					close(terminal)
				}()
			})
			<-ctx.Done()

			return ctx.Err()
		},
	}
	incomplete := errors.New("containment incomplete sentinel")
	transport := &ProcessTransport{
		options: Options{Authority: &NativeAuthority{ContainmentIncomplete: incomplete}},
		process: process,
		stdin:   process.stdin,
		stdout:  process.stdout,
		stderr:  process.stderr,
	}
	err := transport.Close()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, incomplete)
}

func TestRunNativeOutputPreparesRevokesWaitsAndReclaims(t *testing.T) {
	unavailable := errors.New("unavailable sentinel")
	incomplete := errors.New("incomplete sentinel")
	busy := errors.New("busy sentinel")
	var mu sync.Mutex
	events := []string{}
	revoked := make(chan struct{})
	authority := &NativeAuthority{
		Unavailable: unavailable, ContainmentIncomplete: incomplete, TreeBusy: busy,
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(context.Context, string) error {
			mu.Lock()
			events = append(events, "prepare")
			mu.Unlock()

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error {
			mu.Lock()
			events = append(events, "reclaim")
			mu.Unlock()

			return nil
		},
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			mu.Lock()
			events = append(events, "start")
			mu.Unlock()

			return &authorityTestProcess{
				stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
				wait: func(ctx context.Context) (NativeResult, error) {
					select {
					case <-revoked:
						mu.Lock()
						events = append(events, "wait-terminal")
						mu.Unlock()

						return NativeResult{Revoked: true}, nil
					case <-ctx.Done():
						mu.Lock()
						events = append(events, "wait-canceled")
						mu.Unlock()

						return NativeResult{}, ctx.Err()
					}
				},
				revoke: func(context.Context) error {
					mu.Lock()
					events = append(events, "revoke")
					mu.Unlock()
					close(revoked)

					return nil
				},
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, result, err := runNativeOutput(ctx, Options{CLIPath: "claude", ClaudeHome: "/native/home", Authority: authority}, "claude", []string{"auth", "status"})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, result.Revoked)
	require.Equal(t, []string{"prepare", "start", "revoke", "wait-terminal", "reclaim"}, events)
}

func TestRunNativeOutputReclaimsPreparedTreeOnOrdinaryStartRefusal(t *testing.T) {
	refusal := errors.New("native admission refused")
	incomplete := errors.New("containment incomplete sentinel")
	var events []string
	authority := &NativeAuthority{
		Unavailable:           errors.New("authority unavailable sentinel"),
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(context.Context, string) error {
			events = append(events, "prepare")

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error {
			events = append(events, "reclaim")

			return nil
		},
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			events = append(events, "start")

			return nil, refusal
		},
	}

	_, _, err := runNativeOutput(t.Context(), Options{
		CLIPath: "claude", ClaudeHome: "/native/home", Authority: authority,
	}, "claude", []string{"auth", "status"})
	require.Same(t, refusal, err)
	require.NotErrorIs(t, err, incomplete)
	require.Equal(t, []string{"prepare", "start", "reclaim"}, events)
}

func TestRunNativeOutputRetainsPreparedTreeOnExplicitStartAmbiguity(t *testing.T) {
	for _, name := range []string{"authority unavailable", "containment incomplete"} {
		t.Run(name, func(t *testing.T) {
			unavailable := errors.New("authority unavailable sentinel")
			incomplete := errors.New("containment incomplete sentinel")
			startErr := unavailable
			if name == "containment incomplete" {
				startErr = incomplete
			}

			var events []string
			authority := &NativeAuthority{
				Unavailable:           unavailable,
				ContainmentIncomplete: incomplete,
				NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
				PrepareNativeTree: func(context.Context, string) error {
					events = append(events, "prepare")

					return nil
				},
				ReclaimNativeTree: func(context.Context, string) error {
					events = append(events, "reclaim")

					return nil
				},
				StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
					events = append(events, "start")

					return nil, startErr
				},
			}

			_, _, err := runNativeOutput(t.Context(), Options{
				CLIPath: "claude", ClaudeHome: "/native/home", Authority: authority,
			}, "claude", []string{"auth", "status"})
			require.Same(t, startErr, err)
			require.Equal(t, []string{"prepare", "start"}, events)
		})
	}
}

func TestRunNativeOutputRecoversStreamReaderPanics(t *testing.T) {
	for _, test := range []struct {
		name   string
		stdout io.ReadCloser
		stderr io.ReadCloser
		want   error
	}{
		{name: "stdout", stdout: transportPanicReader{}, stderr: io.NopCloser(bytes.NewReader(nil)), want: errClaudeStdoutReaderPanic},
		{name: "stderr", stdout: io.NopCloser(bytes.NewReader(nil)), stderr: transportPanicReader{}, want: errClaudeTransportFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := &NativeAuthority{
				NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
				StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
					return &authorityTestProcess{
						stdin: &authorityTestWriteCloser{}, stdout: test.stdout, stderr: test.stderr,
						wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
						revoke: func(context.Context) error { return nil },
					}, nil
				},
			}

			_, _, err := runNativeOutput(t.Context(), Options{Authority: authority, TreePrepared: true}, "claude", nil)
			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestManagedNativeFailuresUseConfiguredSentinel(t *testing.T) {
	unavailable := errors.New("exact unavailable")
	authority := &NativeAuthority{
		Unavailable:       unavailable,
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			return nil, nil //nolint:nilnil // The invalid success result is the behavior under test.
		},
	}
	_, err := startNative(t.Context(), Options{Authority: authority}, "claude", nil)
	require.ErrorIs(t, err, unavailable)
}

func TestUnusableManagedProcessSettlementIsBounded(t *testing.T) {
	priorShutdown := processShutdownWaitDelay
	processShutdownWaitDelay = 5 * time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = priorShutdown })

	release := make(chan struct{})
	process := &authorityTestProcess{
		stdin:  &authorityTestWriteCloser{},
		stdout: nil,
		stderr: io.NopCloser(bytes.NewReader(nil)),
		wait: func(context.Context) (NativeResult, error) {
			<-release

			return NativeResult{}, nil
		},
		revoke: func(context.Context) error {
			<-release

			return nil
		},
	}
	incomplete := errors.New("incomplete sentinel")
	authority := &NativeAuthority{
		Unavailable:           errors.New("unavailable sentinel"),
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		StartNative:           func(context.Context, NativeRequest) (NativeProcess, error) { return process, nil },
	}

	done := make(chan error, 1)
	go func() {
		_, err := startNative(context.Background(), Options{Authority: authority}, "claude", nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, incomplete)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("unusable managed process settlement did not honor its bound")
	}

	close(release)
}

func TestUnusableManagedProcessTerminalWaitAvoidsFalseContainmentFailure(t *testing.T) {
	unavailable := errors.New("unavailable sentinel")
	incomplete := errors.New("incomplete sentinel")
	process := &authorityTestProcess{
		stdin:  &authorityTestWriteCloser{},
		stdout: nil,
		stderr: io.NopCloser(bytes.NewReader(nil)),
		wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
		revoke: func(context.Context) error { return context.DeadlineExceeded },
	}
	authority := &NativeAuthority{
		Unavailable:           unavailable,
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		StartNative:           func(context.Context, NativeRequest) (NativeProcess, error) { return process, nil },
	}

	_, err := startNative(t.Context(), Options{Authority: authority}, "claude", nil)
	require.ErrorIs(t, err, unavailable)
	require.NotErrorIs(t, err, incomplete)
}

func TestAuthAndUsageLaunchThroughAuthority(t *testing.T) {
	var requests []NativeRequest
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(context.Context, string) error { return nil },
		ReclaimNativeTree: func(context.Context, string) error { return nil },
		StartNative: func(_ context.Context, request NativeRequest) (NativeProcess, error) {
			requests = append(requests, request)
			output := `{"loggedIn":true}`
			if len(request.Arguments) > 0 && request.Arguments[0] == "/usage" {
				output = `{"is_error":false,"result":""}`
			}

			return &authorityTestProcess{stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewBufferString(output)), stderr: io.NopCloser(bytes.NewReader(nil)), wait: func(context.Context) (NativeResult, error) { return NativeResult{}, nil }, revoke: func(context.Context) error { return nil }}, nil
		},
	}
	options := Options{CLIPath: "claude", ClaudeHome: "/native/home", Authority: authority}
	account, _, err := AuthStatus(t.Context(), options)
	require.NoError(t, err)
	require.True(t, account.LoggedIn)
	_, err = QueryRateLimits(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, requests, 2)
	require.Equal(t, []string{"auth", "status", "--json"}, requests[0].Arguments)
	require.Equal(t, "/usage", requests[1].Arguments[0])
}

func TestAuthLoginPreparesHomeAndShimBeforeAuthorityLaunch(t *testing.T) {
	const authorizeURL = "https://claude.com/oauth/authorize?redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback"
	var events []string
	revoked := make(chan struct{})
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(_ context.Context, root string) error {
			events = append(events, "prepare:"+root)

			return nil
		},
		ReclaimNativeTree: func(_ context.Context, root string) error {
			events = append(events, "reclaim:"+root)

			return nil
		},
		StartNative: func(_ context.Context, request NativeRequest) (NativeProcess, error) {
			events = append(events, "start:"+request.Arguments[0]+" "+request.Arguments[1])

			return &authorityTestProcess{
				stdin:  &authorityTestWriteCloser{},
				stdout: io.NopCloser(bytes.NewBufferString("Opening browser to sign in…\n" + authorizeURL + "\n" + AuthLoginPrompt)),
				stderr: io.NopCloser(bytes.NewReader(nil)),
				wait: func(ctx context.Context) (NativeResult, error) {
					select {
					case <-revoked:
						return NativeResult{Revoked: true}, nil
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
	home := t.TempDir()
	login, gotURL, err := StartAuthLogin(t.Context(), Options{CLIPath: "claude", ClaudeHome: home, ScratchParent: t.TempDir(), Authority: authority})
	require.NoError(t, err)
	require.Equal(t, authorizeURL, gotURL)
	require.Len(t, events, 3)
	require.Equal(t, "prepare:"+home, events[0])
	require.Contains(t, events[1], "prepare:")
	require.Equal(t, "start:auth login", events[2])
	require.NoError(t, login.Close())
	require.Len(t, events, 5)
	require.Contains(t, events[3], "reclaim:")
	require.Equal(t, "reclaim:"+home, events[4])
}

func TestAuthLoginOrdinaryStartRefusalReclaimsRootsAndRemovesShim(t *testing.T) {
	refusal := errors.New("native admission refused")
	incomplete := errors.New("containment incomplete sentinel")
	var events []string
	authority := &NativeAuthority{
		Unavailable:           errors.New("authority unavailable sentinel"),
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(_ context.Context, root string) error {
			events = append(events, "prepare:"+root)

			return nil
		},
		ReclaimNativeTree: func(_ context.Context, root string) error {
			events = append(events, "reclaim:"+root)

			return nil
		},
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			events = append(events, "start")

			return nil, refusal
		},
	}
	home := t.TempDir()
	scratch := t.TempDir()

	_, _, err := StartAuthLogin(t.Context(), Options{
		CLIPath: "claude", ClaudeHome: home, ScratchParent: scratch, Authority: authority,
	})
	require.ErrorIs(t, err, refusal)
	require.NotErrorIs(t, err, incomplete)
	require.Len(t, events, 5)
	require.Equal(t, "prepare:"+home, events[0])
	require.Contains(t, events[1], "prepare:"+scratch)
	require.Equal(t, "start", events[2])
	require.Contains(t, events[3], "reclaim:"+scratch)
	require.Equal(t, "reclaim:"+home, events[4])
	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestAuthLoginRetainsPreparedRootsWhenLaterPrepareIsUncertain(t *testing.T) {
	var events []string
	incomplete := errors.New("incomplete sentinel")
	authority := &NativeAuthority{
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree: func(_ context.Context, root string) error {
			events = append(events, "prepare:"+root)
			if len(events) == 2 {
				return errors.New("prepare response lost")
			}

			return nil
		},
		ReclaimNativeTree: func(_ context.Context, root string) error {
			events = append(events, "reclaim:"+root)

			return nil
		},
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			events = append(events, "start")

			return nil, errors.New("unexpected start")
		},
	}

	home := t.TempDir()
	_, _, err := StartAuthLogin(t.Context(), Options{
		CLIPath: "claude", ClaudeHome: home, ScratchParent: t.TempDir(), Authority: authority,
	})
	require.ErrorIs(t, err, incomplete)
	require.Len(t, events, 2)
	require.Equal(t, "prepare:"+home, events[0])
	require.Contains(t, events[1], "prepare:")
}

func TestAuthLoginPrepareFailureRemovesOnlyTheUnpreparedShim(t *testing.T) {
	scratch := t.TempDir()
	incomplete := errors.New("incomplete sentinel")
	authority := &NativeAuthority{
		ContainmentIncomplete: incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"PATH": "/native/bin"} },
		PrepareNativeTree:     func(context.Context, string) error { return errors.New("prepare response lost") },
	}

	_, _, err := StartAuthLogin(t.Context(), Options{
		CLIPath: "claude", ClaudeHome: t.TempDir(), ScratchParent: scratch, Authority: authority,
	})
	require.ErrorIs(t, err, incomplete)
	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}
