package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type authorityTestWriteCloser struct{ bytes.Buffer }

func (*authorityTestWriteCloser) Close() error { return nil }

type authorityTestProcess struct {
	stdin  *authorityTestWriteCloser
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
	require.Equal(t, []string{"prepare", "start", "wait-canceled", "revoke", "wait-terminal", "reclaim"}, events)
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
