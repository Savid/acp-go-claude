package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func newRateLimitsFixtureAgent(t *testing.T) *Agent {
	t.Helper()

	agent := NewAgent(WithHome(t.TempDir()))
	agent.ordinaryEnv = map[string]string{}
	agent.queryRateLimits = func(context.Context, *claude.Client) (claude.RateLimits, error) {
		return claude.RateLimits{}, nil
	}
	for _, id := range []acp.SessionId{"session-1", "😀: session "} {
		agent.sessions[id] = &agentSession{
			agent: agent, id: id,
			client:        claude.NewClient(nil, claude.Options{}, nil),
			clientOptions: claude.Options{OrdinaryEnvironment: map[string]string{}},
		}
	}

	return agent
}

func TestHandleRateLimitsSelection(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		params       string
		availability string
		reason       string
	}{
		{name: "native home needs session", params: `{}`, availability: "unavailable", reason: "session_required"},
		{name: "native session no data", params: `{"sessionId":"session-1"}`, availability: "unavailable", reason: "not_observed"},
		{name: "unknown provider", params: `{"providerId":" opaque-provider "}`, availability: "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent := newRateLimitsFixtureAgent(t)
			got, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(test.params))
			require.NoError(t, err)
			result, ok := got.(RateLimitsResponse)
			require.True(t, ok)
			require.Equal(t, test.availability, result.Availability)
			require.Equal(t, test.reason, result.Reason)
			require.NotNil(t, result.Pools)
			require.Empty(t, result.Pools)
		})
	}
}

func TestHandleRateLimitsValidatesAndResolvesBeforeReading(t *testing.T) {
	t.Parallel()
	agent := newRateLimitsFixtureAgent(t)
	agent.queryRateLimits = func(context.Context, *claude.Client) (claude.RateLimits, error) {
		t.Fatal("invalid request reached native reader")

		return claude.RateLimits{}, nil
	}
	_, lifecycleErr := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod,
		json.RawMessage(`{"sessionId":"session-1","_meta":{"acp-go.dev/lifecycle":{}}}`))
	requireExactUnsupportedField(t, lifecycleErr, `_meta["acp-go.dev/lifecycle"]`)

	for _, raw := range []string{
		`{"sessionId":"session-1","unexpected":1}`,
		`{"sessionId":"missing","providerId":"unsupported"}`,
	} {
		_, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(raw))
		require.Error(t, err)
	}
	agent.sessions["session-1"].closing = true
	_, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1","providerId":"unsupported"}`))
	require.Error(t, err)
	agent.sessions["session-1"].closing = false
	agent.sessions["session-1"].poisonCause = poisonCauseSessionIDDrift
	_, err = agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
	requirePoisonedSession(t, err, poisonCauseSessionIDDrift)
}

func TestHandleRateLimitsAuthorityFailureFencesAgent(t *testing.T) {
	t.Parallel()

	agent := newRateLimitsFixtureAgent(t)
	agent.queryRateLimits = func(context.Context, *claude.Client) (claude.RateLimits, error) {
		return claude.RateLimits{}, ErrHostAuthorityUnavailable
	}
	_, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, agent.Close(), ErrHostAuthorityUnavailable)
}

func TestHandleRateLimitsNativeDataAndFreshness(t *testing.T) {
	t.Parallel()
	agent := newRateLimitsFixtureAgent(t)
	observedAt := time.Now().UTC()
	calls := 0
	agent.queryRateLimits = func(ctx context.Context, client *claude.Client) (claude.RateLimits, error) {
		require.Same(t, agent.sessions["session-1"].client, client)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		calls++

		return claude.RateLimits{ObservedAt: observedAt, PlanType: "max", Pools: []claude.RateLimitPool{{
			ID: "subscription", Windows: []claude.RateLimitWindow{
				{ID: "five_hour", UsedPercent: 125.5, ResetsAt: observedAt.Add(time.Hour)},
				{ID: "seven_day", UsedPercent: 0},
			},
		}}}, nil
	}
	for range 2 {
		result, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
		require.NoError(t, err)
		require.Equal(t, "available", result.Availability)
		require.Len(t, result.Pools, 1)
		require.Len(t, result.Pools[0].Windows, 2)
		require.Equal(t, 125.5, *result.Pools[0].Windows[0].UsedPercent)
		require.Equal(t, 0.0, *result.Pools[0].Windows[1].UsedPercent)
		require.Equal(t, observedAt.Format(time.RFC3339Nano), result.Pools[0].Windows[0].ObservedAt)
	}
	require.Equal(t, 2, calls)
	observedAt = observedAt.Add(-time.Minute)
	result, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
	require.NoError(t, err)
	require.Equal(t, "not_observed", result.Reason)
}

func TestNormalizeRateLimitsPreservesKnownSourceDuration(t *testing.T) {
	t.Parallel()
	result := normalizeRateLimits("anthropic", claude.RateLimits{ObservedAt: time.Now(), Pools: []claude.RateLimitPool{
		{ID: "model:Fable", Label: "Fable", Windows: []claude.RateLimitWindow{{ID: "usage", UsedPercent: 51, DurationSeconds: 604800}}},
		{ID: "model:other", Windows: []claude.RateLimitWindow{{ID: "usage", UsedPercent: 10}}},
	}})
	require.Len(t, result.Pools, 2)
	require.NotNil(t, result.Pools[0].Windows[0].DurationSeconds)
	require.Equal(t, int64(604800), *result.Pools[0].Windows[0].DurationSeconds)
	require.Nil(t, result.Pools[1].Windows[0].DurationSeconds)
}

func TestHandleRateLimitsFailuresAndCallerCancellation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		err       error
		reason    string
		wireError bool
	}{
		{name: "native failure", err: errors.New("native unavailable"), reason: "read_failed"},
		{name: "source timeout", err: context.DeadlineExceeded, reason: "read_failed"},
		{name: "probe disabled", err: claude.ErrRateLimitsDisabled, reason: "disabled"},
		{name: "probe authentication", err: fmt.Errorf("probe: %w", claude.ErrRateLimitsNotAuthenticated), reason: "not_authenticated"},
		{name: "authority", err: ErrHostAuthorityUnavailable, wireError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent := newRateLimitsFixtureAgent(t)
			agent.queryRateLimits = func(context.Context, *claude.Client) (claude.RateLimits, error) { return claude.RateLimits{}, test.err }
			result, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
			if test.wireError {
				require.ErrorIs(t, err, test.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "unavailable", result.Availability)
				require.Equal(t, test.reason, result.Reason)
				require.NotNil(t, result.Pools)
				require.Empty(t, result.Pools)
			}
		})
	}
	agent := newRateLimitsFixtureAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	agent.queryRateLimits = func(ctx context.Context, _ *claude.Client) (claude.RateLimits, error) {
		cancel()
		<-ctx.Done()

		return claude.RateLimits{}, ctx.Err()
	}
	_, err := agent.handleRateLimits(ctx, json.RawMessage(`{"sessionId":"session-1"}`))
	require.ErrorIs(t, err, context.Canceled)
}

func TestHandleRateLimitsFencesChangedTarget(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"auth", "client", "environment", "close"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			agent := newRateLimitsFixtureAgent(t)
			started, release := make(chan struct{}), make(chan struct{})
			agent.queryRateLimits = func(context.Context, *claude.Client) (claude.RateLimits, error) {
				close(started)
				<-release

				return claude.RateLimits{ObservedAt: time.Now(), Pools: []claude.RateLimitPool{{ID: "subscription", Windows: []claude.RateLimitWindow{{ID: "five_hour", UsedPercent: 6}}}}}, nil
			}
			done := make(chan struct{})
			var result RateLimitsResponse
			var err error
			go func() {
				result, err = agent.handleRateLimits(t.Context(), json.RawMessage(`{"sessionId":"session-1"}`))
				close(done)
			}()
			<-started
			session := agent.sessions["session-1"]
			session.mu.Lock()
			switch mutation {
			case "auth":
				agent.invalidateProviderObservations()
			case "client":
				session.client = nil
			case "environment":
				session.clientOptions.Env = map[string]string{"ANTHROPIC_API_KEY": "new"}
			case "close":
				session.closing = true
			}
			session.mu.Unlock()
			close(release)
			<-done
			if mutation == "close" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "not_observed", result.Reason)
			}
		})
	}
}

func TestRateLimitsProbeUsesSessionScratchDirectory(t *testing.T) {
	for _, pathKind := range []string{"absolute", "relative"} {
		t.Run(pathKind, func(t *testing.T) {
			workingDirectory := t.TempDir()
			t.Chdir(workingDirectory)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)

					return
				}
				calls.Add(1)
				var request struct {
					Model string `json:"model"`
				}
				if json.NewDecoder(r.Body).Decode(&request) == nil && request.Model == "claude-fable-5-1" {
					w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "1.0")
				}
				w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.57")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(upstream.Close)

			scratch, cwd := filepath.Join(workingDirectory, "scratch"), t.TempDir()
			scratchOption := scratch
			if pathKind == "relative" {
				scratchOption = "scratch"
			}
			launches := make(chan NativeRequest, 1)
			requestErrors := make(chan error, 1)
			authority := newFakeHostAuthority()
			authority.start = func(ctx context.Context, request NativeRequest) (NativeProcess, error) {
				if request.Executable == keychainToolExecutable {
					return valueNativeProcess{}, nil
				}
				if slices.Equal(request.Arguments, []string{"--version"}) {
					return &fakeNativeProcess{authority: authority, stdout: io.NopCloser(strings.NewReader("2.1.263\n"))}, nil
				}
				if slices.Contains(request.Arguments, "--safe-mode") {
					launches <- request

					return newQuotaProbeNativeProcess(ctx, authority, request, requestErrors), nil
				}

				return newConfigurationNativeProcess(), nil
			}
			agent := NewAgent(WithHome(t.TempDir()), WithScratchDir(scratchOption), WithExecutablePath("fixture-claude"),
				WithHostAuthority(authority), WithLogger(slog.New(slog.DiscardHandler)), WithEnv(map[string]string{
					"CLAUDE_CODE_OAUTH_TOKEN": "synthetic-quota-token", "ANTHROPIC_BASE_URL": upstream.URL,
				}))
			agent.setConnection(newRecordingAgentClient())
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			session, err := agent.NewSession(t.Context(), NewSessionRequest(cwd))
			require.NoError(t, err)
			// Removing the unused fake-native CWD makes misplaced scratch creation fail
			// deterministically, including when tests run as root.
			require.NoError(t, os.Remove(cwd))
			params, err := json.Marshal(RateLimitsRequest{SessionID: session.SessionId})
			require.NoError(t, err)
			value, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, params)
			require.NoError(t, err)
			result, ok := value.(RateLimitsResponse)
			require.True(t, ok)
			require.Equal(t, "available", result.Availability)
			var fable *RateLimitPool
			for i := range result.Pools {
				if result.Pools[i].ID == "model:Fable" {
					fable = &result.Pools[i]
				}
			}
			require.NotNil(t, fable, "the native 429 must retain the exhausted Fable pool")
			require.Len(t, fable.Windows, 1)
			require.Equal(t, 100.0, *fable.Windows[0].UsedPercent)
			require.Equal(t, int32(1), calls.Load(), "usable Fable headers must not trigger a Haiku fallback")
			request := <-launches
			root := filepath.Dir(request.WorkingDirectory)
			require.Equal(t, scratch, filepath.Dir(root))
			require.NoDirExists(t, root)
			require.Contains(t, authority.snapshot(), "reclaim:"+root)
			require.NoError(t, <-requestErrors)
		})
	}
}

func newQuotaProbeNativeProcess(ctx context.Context, authority *fakeHostAuthority, request NativeRequest, requestErrors chan<- error) NativeProcess {
	var baseURL, token string
	for _, entry := range request.Environment {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "ANTHROPIC_BASE_URL":
			baseURL = value
		case "CLAUDE_CODE_OAUTH_TOKEN":
			token = value
		}
	}
	done := make(chan struct{})
	stdout, output := io.Pipe()
	go func() {
		defer close(done)
		defer output.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages?beta=true",
			strings.NewReader(`{"model":"claude-fable-5-1","max_tokens":1,"tools":[]}`))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
			var response *http.Response
			response, err = http.DefaultClient.Do(req)
			if err == nil {
				_ = response.Body.Close()
			}
		}
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		requestErrors <- err
	}()

	return &fakeNativeProcess{authority: authority, stdout: stdout, waitFor: done}
}
