package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimitsNativeProbeUsesCapturedIdentityAndSettlesBeforeReclaim(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.4")
	}))
	t.Cleanup(upstream.Close)
	fixture := newRateLimitsNativeFixture(t, upstream.URL)
	result, err := fixture.client.readRateLimitsNativeProbe(t.Context(), fixture.access)
	require.NoError(t, err)
	require.Len(t, result.Pools, 1)
	require.Equal(t, "model:Fable", result.Pools[0].ID)
	require.Equal(t, 40.0, result.Pools[0].Windows[0].UsedPercent)
	require.Equal(t, int32(1), calls.Load())
	require.NoError(t, <-fixture.requestErr)
	require.True(t, fixture.settled.Load())
	require.Equal(t, int32(1), fixture.reclaimed.Load())
	require.NoDirExists(t, fixture.root)
	require.Empty(t, fixture.client.rateLimitsProbes)
	require.Equal(t, []string{"--version"}, fixture.requests[0].Arguments)
	require.Contains(t, fixture.requests[1].Arguments, "fixture-fable")
	require.Contains(t, fixture.requests[1].Arguments, "--safe-mode")
	require.Contains(t, fixture.requests[1].Arguments, "--no-session-persistence")
	env := rateLimitsFixtureEnvironment(fixture.requests[1].Environment)
	require.Equal(t, "fixture-token", env[rateLimitsSetupToken])
	require.Equal(t, "1", env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"])
	require.Equal(t, "0", env["CLAUDE_CODE_MAX_RETRIES"])
	require.Empty(t, env["HTTPS_PROXY"])
	require.Equal(t, filepath.Join(fixture.root, "home"), env["HOME"])
	require.Equal(t, filepath.Join(fixture.root, "config"), env["CLAUDE_CONFIG_DIR"])
	require.Equal(t, filepath.Join(fixture.root, "work"), fixture.requests[1].WorkingDirectory)
	require.Equal(t, upstream.URL, fixture.transport.env[rateLimitsBaseURL])
	entries, err := os.ReadDir(fixture.client.options.Cwd)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRateLimitsNativeProbeRetainsFailedReclaimForClientClose(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
	}))
	t.Cleanup(upstream.Close)
	fixture := newRateLimitsNativeFixture(t, upstream.URL)
	fixture.failReclaim.Store(true)
	_, err := fixture.client.readRateLimitsNativeProbe(t.Context(), fixture.access)
	require.ErrorIs(t, err, fixture.incomplete)
	require.True(t, fixture.settled.Load())
	require.DirExists(t, fixture.root)
	require.Len(t, fixture.client.rateLimitsProbes, 1)
	require.NoError(t, <-fixture.requestErr)
	fixture.failReclaim.Store(false)
	require.NoError(t, fixture.client.Close())
	require.NoDirExists(t, fixture.root)
	require.Empty(t, fixture.client.rateLimitsProbes)
	require.Equal(t, int32(2), fixture.reclaimed.Load())
}

func TestRateLimitsNativeProbeDoesNotTouchPreparedParentOrChangedIdentity(t *testing.T) {
	t.Parallel()
	fixture := newRateLimitsNativeFixture(t, "https://fixture.invalid")
	fixture.client.options.ClaudeHome = fixture.client.options.Cwd
	result, err := fixture.client.readRateLimitsNativeProbe(t.Context(), fixture.access)
	require.NoError(t, err)
	require.Empty(t, result.Pools)
	require.Empty(t, fixture.requests)
	fixture.client.options.ClaudeHome = ""
	fixture.transport.env[rateLimitsSetupToken] = "different-fixture-token"
	result, err = fixture.client.readRateLimitsNativeProbe(t.Context(), fixture.access)
	require.NoError(t, err)
	require.Empty(t, result.Pools)
	require.Empty(t, fixture.requests)
	entries, err := os.ReadDir(fixture.client.options.Cwd)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRateLimitsNativeProbeClientCloseCancelsAndSettlesActiveRead(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	fixture := newRateLimitsNativeFixture(t, upstream.URL)
	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.readRateLimitsNativeProbe(t.Context(), fixture.access)
		done <- err
	}()
	<-entered
	require.NoError(t, fixture.client.Close())
	require.Error(t, <-done)
	require.NoError(t, <-fixture.requestErr)
	require.True(t, fixture.settled.Load())
	require.NoDirExists(t, fixture.root)
	require.Empty(t, fixture.client.rateLimitsProbes)
}

func TestRateLimitsNativeProbeQueuedReadHonorsCancellation(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	fixture := newRateLimitsNativeFixture(t, upstream.URL)
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	t.Cleanup(cancelFirst)
	firstDone := make(chan error, 1)
	go func() {
		_, err := fixture.client.readRateLimitsNativeProbe(firstCtx, fixture.access)
		firstDone <- err
	}()
	<-entered
	queuedCtx, cancelQueued := context.WithCancel(t.Context())
	defer cancelQueued()
	queuedStarted := make(chan struct{})
	queuedDone := make(chan error, 1)
	go func() {
		close(queuedStarted)
		_, err := fixture.client.readRateLimitsNativeProbe(queuedCtx, fixture.access)
		queuedDone <- err
	}()
	<-queuedStarted
	cancelQueued()
	select {
	case err := <-queuedDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("queued probe ignored cancellation while another probe was active")
	}
	select {
	case err := <-firstDone:
		t.Fatalf("queued cancellation settled the active probe: %v", err)
	default:
	}
	require.False(t, fixture.settled.Load())
	require.Zero(t, fixture.reclaimed.Load())
	cancelFirst()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	require.NoError(t, <-fixture.requestErr)
	require.True(t, fixture.settled.Load())
	require.NoDirExists(t, fixture.root)
}

func TestReadRateLimitsFallbackUsesHaikuOnlyAfterEmptyFable(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fable quota", "fable unavailable", "fable unauthorized", "identity changed", "reclaim failed"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var fableCalls, haikuCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				if strings.Contains(string(body), "fixture-fable") {
					fableCalls.Add(1)
					if mode == "fable quota" {
						w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.51")
					}
					if mode == "fable unauthorized" {
						w.WriteHeader(http.StatusUnauthorized)
					}

					return
				}
				haikuCalls.Add(1)
				require.Contains(t, string(body), "claude-haiku-4-5")
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
			}))
			t.Cleanup(upstream.Close)
			fixture := newRateLimitsNativeFixture(t, upstream.URL)
			fixture.failReclaim.Store(mode == "reclaim failed")
			go autoRespondInitialize(fixture.transport.fakeTransport)
			startClientForTest(t, fixture.client)
			go respondToControlRequestWithResponse(fixture.transport.fakeTransport, "get_usage", map[string]any{"rate_limits_available": false})
			go func() {
				settings := map[string]any{"effective": map[string]any{}}
				respondToControlRequestWithResponse(fixture.transport.fakeTransport, "get_settings", settings)
				if mode == "reclaim failed" || mode == "fable unauthorized" {
					return
				}
				if mode == "identity changed" {
					settings = map[string]any{"effective": map[string]any{"apiKeyHelper": "changed-helper"}}
				}
				respondToControlRequestAfter(fixture.transport.fakeTransport, "get_settings", 3, settings)
				if mode == "fable unavailable" {
					respondToControlRequestAfter(fixture.transport.fakeTransport, "get_settings", 4, settings)
				}
			}()
			result, err := fixture.client.ReadRateLimitsWithFallback(t.Context(), true)
			fixture.failReclaim.Store(false)
			switch mode {
			case "reclaim failed":
				require.ErrorIs(t, err, fixture.incomplete)
			case "fable unauthorized":
				require.ErrorIs(t, err, ErrRateLimitsNotAuthenticated)
			default:
				require.NoError(t, err)
			}
			require.Equal(t, int32(1), fableCalls.Load())
			if mode == "fable unavailable" {
				require.Equal(t, int32(1), haikuCalls.Load())
				require.Equal(t, "subscription", result.Pools[0].ID)
			} else {
				require.Zero(t, haikuCalls.Load())
			}
			if mode == "fable quota" {
				require.Equal(t, "model:Fable", result.Pools[0].ID)
			} else if mode != "fable unavailable" {
				require.Empty(t, result.Pools)
			}
		})
	}
}

type rateLimitsNativeFixture struct {
	client      *Client
	transport   *rateLimitsAPITransport
	access      rateLimitsAPIAccess
	root        string
	requests    []NativeRequest
	requestErr  chan error
	settled     atomic.Bool
	reclaimed   atomic.Int32
	failReclaim atomic.Bool
	incomplete  error
}

func newRateLimitsNativeFixture(t *testing.T, endpoint string) *rateLimitsNativeFixture {
	t.Helper()
	fixture := &rateLimitsNativeFixture{
		requestErr: make(chan error, 1), incomplete: errors.New("quota fixture containment incomplete"),
		transport: &rateLimitsAPITransport{fakeTransport: newFakeTransport(), env: map[string]string{
			rateLimitsSetupToken: "fixture-token", rateLimitsBaseURL: endpoint,
			"ANTHROPIC_DEFAULT_FABLE_MODEL": "fixture-fable", "HTTPS_PROXY": "https://unused.invalid",
		}},
	}
	var eligible bool
	fixture.access, eligible = resolveRateLimitsAPIAccess(fixture.transport.env)
	require.True(t, eligible)
	authority := &NativeAuthority{
		ContainmentIncomplete: fixture.incomplete,
		NativeEnvironment:     func() map[string]string { return map[string]string{"SHOULD_NOT_BE_READ": "different-identity"} },
		PrepareNativeTree: func(_ context.Context, root string) error {
			fixture.root = root

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error {
			fixture.reclaimed.Add(1)
			if !fixture.settled.Load() {
				return errors.New("reclaimed before native settlement")
			}
			if fixture.failReclaim.Load() {
				return errors.New("fixture reclaim refused")
			}

			return nil
		},
		StartNative: func(ctx context.Context, request NativeRequest) (NativeProcess, error) {
			fixture.requests = append(fixture.requests, request)
			if len(request.Arguments) == 1 && request.Arguments[0] == "--version" {
				return &authorityTestProcess{
					stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(strings.NewReader("2.1.263\n")), stderr: io.NopCloser(bytes.NewReader(nil)),
					wait: func(context.Context) (NativeResult, error) { return NativeResult{}, nil }, revoke: func(context.Context) error { return nil },
				}, nil
			}

			return fixture.process(ctx, request), nil
		},
	}
	fixture.client = NewClient(nil, Options{CLIPath: "fixture-claude", Cwd: t.TempDir(), Authority: authority, ContainmentIncomplete: fixture.incomplete}, fixture.transport)
	t.Cleanup(func() { require.NoError(t, fixture.client.Close()) })

	return fixture
}

func (f *rateLimitsNativeFixture) process(ctx context.Context, request NativeRequest) NativeProcess {
	done := make(chan struct{})
	stdout, output := io.Pipe()
	var start sync.Once

	return &authorityTestProcess{
		stdin: authorityTestCloseFunc(func() error {
			start.Do(func() {
				go func() {
					defer close(done)
					defer output.Close()
					env := rateLimitsFixtureEnvironment(request.Environment)
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, env[rateLimitsBaseURL]+"/v1/messages?beta=true", strings.NewReader(`{"model":"fixture-fable","max_tokens":1,"tools":[]}`))
					if err == nil {
						req.Header.Set("Authorization", "Bearer "+env[rateLimitsSetupToken])
						var response *http.Response
						response, err = http.DefaultClient.Do(req)
						if err == nil {
							_ = response.Body.Close()
						}
					}
					if errors.Is(err, context.Canceled) {
						err = nil
					}
					f.requestErr <- err
				}()
			})

			return nil
		}),
		stdout: stdout, stderr: io.NopCloser(bytes.NewReader(nil)),
		wait: func(ctx context.Context) (NativeResult, error) {
			select {
			case <-done:
				f.settled.Store(true)

				return NativeResult{}, nil
			case <-ctx.Done():
				return NativeResult{}, ctx.Err()
			}
		},
		revoke: func(context.Context) error { return nil },
	}
}

func rateLimitsFixtureEnvironment(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}

	return result
}
