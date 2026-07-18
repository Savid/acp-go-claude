package claude

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const usagePanelSample = "You are currently using your subscription to power your Claude Code usage\n\n" +
	"Current session: 92% used · resets Jul 9, 1:40pm (Australia/Brisbane)\n" +
	"Current week (all models): 73% used · resets Jul 11, 6am (Australia/Brisbane)\n" +
	"Current week (Fable): 48.5% used · resets Jul 11, 6am (Australia/Brisbane)\n\n" +
	"What's contributing to your limits usage?\n"

func usageTestNow(t *testing.T) time.Time {
	t.Helper()

	location, err := time.LoadLocation("Australia/Brisbane")
	require.NoError(t, err)

	return time.Date(2026, time.July, 9, 10, 0, 0, 0, location)
}

func TestParseUsageWindows(t *testing.T) {
	now := usageTestNow(t)

	windows := parseUsageWindows(usagePanelSample, now)
	require.Len(t, windows, 3)

	require.Equal(t, "session", windows[0].ID)
	require.InDelta(t, 92.0, windows[0].UsedPercent, 0.0001)
	require.Equal(t, "2026-07-09T13:40:00+10:00", windows[0].ResetsAt.Format(time.RFC3339))

	require.Equal(t, "week-all-models", windows[1].ID)
	require.InDelta(t, 73.0, windows[1].UsedPercent, 0.0001)
	require.Equal(t, "2026-07-11T06:00:00+10:00", windows[1].ResetsAt.Format(time.RFC3339))

	require.Equal(t, "week-fable", windows[2].ID)
	require.InDelta(t, 48.5, windows[2].UsedPercent, 0.0001)
}

func TestParseUsageWindowsWithoutUsageLines(t *testing.T) {
	windows := parseUsageWindows("You are on API billing.\nUse /cost for session costs.\n", usageTestNow(t))
	require.Empty(t, windows)
}

func TestParseUsageWindowsWithoutReset(t *testing.T) {
	windows := parseUsageWindows("Current session: 12% used\n", usageTestNow(t))
	require.Len(t, windows, 1)
	require.Equal(t, "session", windows[0].ID)
	require.InDelta(t, 12.0, windows[0].UsedPercent, 0.0001)
	require.True(t, windows[0].ResetsAt.IsZero())
}

func TestUsageWindowID(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{label: "session", want: "session"},
		{label: "week (all models)", want: "week-all-models"},
		{label: "week (Fable)", want: "week-fable"},
		{label: "  Week  ", want: "week"},
	}

	for _, tc := range tests {
		require.Equal(t, tc.want, usageWindowID(tc.label), tc.label)
	}
}

func TestParseUsageReset(t *testing.T) {
	now := usageTestNow(t)

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "minutes and location",
			text: "Jul 9, 1:40pm (Australia/Brisbane)",
			want: "2026-07-09T13:40:00+10:00",
		},
		{
			name: "hour only",
			text: "Jul 11, 6am (Australia/Brisbane)",
			want: "2026-07-11T06:00:00+10:00",
		},
		{
			name: "year rollover",
			text: "Jan 2, 6am (Australia/Brisbane)",
			want: "2027-01-02T06:00:00+10:00",
		},
		{
			name: "no location uses now's location",
			text: "Jul 9, 1:40pm",
			want: "2026-07-09T13:40:00+10:00",
		},
		{name: "unknown location", text: "Jul 9, 1:40pm (Mars/Olympus)", want: ""},
		{name: "unparseable", text: "soon", want: ""},
		{name: "empty", text: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reset := parseUsageReset(tc.text, now)
			if tc.want == "" {
				require.True(t, reset.IsZero())

				return
			}

			require.Equal(t, tc.want, reset.Format(time.RFC3339))
		})
	}
}

func TestParseUsageOutput(t *testing.T) {
	now := usageTestNow(t)

	envelope, err := json.Marshal(map[string]any{"type": "result", "result": usagePanelSample})
	require.NoError(t, err)

	limits, err := parseUsageOutput(envelope, now)
	require.NoError(t, err)
	require.Len(t, limits.Windows, 3)

	_, err = parseUsageOutput([]byte("not json"), now)
	require.ErrorContains(t, err, "parse claude /usage output")

	failed, err := json.Marshal(map[string]any{"is_error": true, "result": "usage lookup failed"})
	require.NoError(t, err)

	_, err = parseUsageOutput(failed, now)
	require.ErrorContains(t, err, "usage lookup failed")

	empty, err := json.Marshal(map[string]any{"is_error": true, "result": "  "})
	require.NoError(t, err)

	_, err = parseUsageOutput(empty, now)
	require.ErrorContains(t, err, "empty result")

	long, err := json.Marshal(map[string]any{"is_error": true, "result": string(make([]byte, 300))})
	require.NoError(t, err)

	_, err = parseUsageOutput(long, now)
	require.ErrorContains(t, err, "claude /usage failed")
}

func TestQueryRateLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()

	envelope, err := json.Marshal(map[string]any{"type": "result", "result": usagePanelSample})
	require.NoError(t, err)

	good := writeShellScript(
		t, filepath.Join(dir, "good"),
		"#!/bin/sh\ncat <<'EOF'\n"+string(envelope)+"\nEOF\n",
	)

	options := platformTestTransportOptions(t, Options{CLIPath: good, Cwd: dir})
	prepareGeneration := options.PrepareUsageGeneration
	acquireDiscovery := options.AcquireUsageDiscovery
	prepared := 0
	admitted := 0
	released := 0
	inventories := 0
	quiesced := 0
	options.PrepareUsageGeneration = func(ctx context.Context) (*DarwinGeneration, error) {
		prepared++

		return prepareGeneration(ctx)
	}
	options.AcquireUsageDiscovery = func(ctx context.Context) (func(), error) {
		admitted++
		release, acquireErr := acquireDiscovery(ctx)

		return func() {
			released++
			release()
		}, acquireErr
	}
	options.ObserveProcessInventory = func(context.Context, func() (int, bool)) { inventories++ }
	options.ObserveProcessQuiesced = func(context.Context) { quiesced++ }
	limits, err := QueryRateLimits(context.Background(), options)
	require.NoError(t, err)
	require.Len(t, limits.Windows, 3)
	require.Equal(t, "session", limits.Windows[0].ID)
	require.Equal(t, 1, prepared)
	require.Equal(t, 1, admitted)
	require.Equal(t, 1, released)
	require.Positive(t, inventories)
	require.Equal(t, 1, quiesced)

	failing := writeShellScript(t, filepath.Join(dir, "fail"), "#!/bin/sh\nexit 1\n")

	options.CLIPath = failing
	_, err = QueryRateLimits(context.Background(), options)
	require.ErrorContains(t, err, "run claude /usage")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	options.CLIPath = good
	_, err = QueryRateLimits(cancelled, options)
	require.Error(t, err)
}

func TestQueryRateLimitsFailsClosedWithoutContainedResources(t *testing.T) {
	_, err := QueryRateLimits(context.Background(), Options{CLIPath: "must-not-run"})
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	options := Options{
		CLIPath: "must-not-run",
		PrepareUsageGeneration: func(context.Context) (*DarwinGeneration, error) {
			return &DarwinGeneration{}, nil
		},
	}
	_, err = QueryRateLimits(context.Background(), options)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.True(t, errors.Is(err, ErrProcessContainmentIncomplete))

	wantErr := errors.New("usage admission failed")
	options.PrepareUsageGeneration = func(context.Context) (*DarwinGeneration, error) {
		return nil, wantErr
	}
	_, err = QueryRateLimits(context.Background(), options)
	require.ErrorIs(t, err, wantErr)

	finished := 0
	options.PrepareUsageGeneration = func(context.Context) (*DarwinGeneration, error) {
		return &DarwinGeneration{Release: func(bool) error {
			finished++

			return nil
		}}, nil
	}
	options.AcquireUsageDiscovery = func(context.Context) (func(), error) { return nil, wantErr }
	_, err = QueryRateLimits(context.Background(), options)
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, finished)

	options.AcquireUsageDiscovery = func(context.Context) (func(), error) { return nil, nil } //nolint:nilnil // Invalid callback result under test.
	_, err = QueryRateLimits(context.Background(), options)
	require.ErrorContains(t, err, "nil release")
	require.Equal(t, 2, finished)
}
