//go:build !windows

package claude

// The tests here drive the claude auth login child through a real browser shim.
// The shim only exists on platforms where a launcher can be neutralised, so on
// Windows the same entry points fail closed before any of this is reachable and
// auth_login_windows_test.go states that refusal instead.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthLoginPrepareAndStartReceiveCallerContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "caller")
	var prepares atomic.Int32
	var starts atomic.Int32
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		PrepareNativeTree: func(callCtx context.Context, _ string) error {
			require.Equal(t, "caller", callCtx.Value(contextKey{}))
			prepares.Add(1)

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error { return nil },
		StartNative: func(callCtx context.Context, _ NativeRequest) (NativeProcess, error) {
			require.Equal(t, "caller", callCtx.Value(contextKey{}))
			starts.Add(1)

			return &authorityTestProcess{
				stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
				wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
				revoke: func(context.Context) error { return nil },
			}, nil
		},
	}
	login, err := startAuthLoginChild(ctx, Options{
		CLIPath: "claude", ClaudeHome: "/home", ScratchParent: t.TempDir(), Authority: authority,
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), prepares.Load())
	require.Equal(t, int32(1), starts.Load())
	require.NoError(t, login.Close())
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

func TestStartAuthLoginResidualFailures(t *testing.T) {
	scratchFile := filepath.Join(t.TempDir(), "scratch-file")
	require.NoError(t, os.WriteFile(scratchFile, []byte("x"), 0o600))
	_, _, err := StartAuthLogin(t.Context(), Options{ScratchParent: scratchFile})
	require.ErrorContains(t, err, "browser launch")

	_, err = startAuthLoginChild(t.Context(), Options{
		ScratchParent: t.TempDir(),
		Authority: &NativeAuthority{
			NativeEnvironment: func() map[string]string { return nil },
		},
	})
	require.Error(t, err)

	_, err = startAuthLoginChild(t.Context(), Options{
		ScratchParent: t.TempDir(),
		Authority: &NativeAuthority{
			NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		},
	})
	require.Error(t, err)

	incomplete := errors.New("incomplete")
	_, err = startAuthLoginChild(t.Context(), Options{
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
	_, err = startAuthLoginChild(t.Context(), Options{ScratchParent: t.TempDir(), Authority: authority, TreePrepared: true})
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

	select {
	case <-readerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the presentation reader never returned after it was released")
	}

	require.ErrorIs(t, err, context.DeadlineExceeded)
}
