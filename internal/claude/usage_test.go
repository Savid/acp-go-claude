package claude

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const currentUsagePanelSample = "You are currently using your subscription to power your Claude Code usage\n\n" +
	"Current session: 92% used · resets Jul 9, 1:40pm (Australia/Brisbane)\n" +
	"Current week (all models): 73% used · resets Jul 11, 6am (Australia/Brisbane)\n" +
	"Current week (Fable): 48.5% used · resets Jul 11, 6am (Australia/Brisbane)\n"

func currentUsageNow(t *testing.T) time.Time {
	t.Helper()
	location, err := time.LoadLocation("Australia/Brisbane")
	require.NoError(t, err)

	return time.Date(2026, time.July, 9, 10, 0, 0, 0, location)
}

func TestParseUsageWindowsCurrentPanel(t *testing.T) {
	windows := parseUsageWindows(currentUsagePanelSample, currentUsageNow(t))
	require.Len(t, windows, 3)
	require.Equal(t, "session", windows[0].ID)
	require.Equal(t, 92.0, windows[0].UsedPercent)
	require.Equal(t, "2026-07-09T13:40:00+10:00", windows[0].ResetsAt.Format(time.RFC3339))
	require.Equal(t, "week-all-models", windows[1].ID)
	require.Equal(t, "2026-07-11T06:00:00+10:00", windows[1].ResetsAt.Format(time.RFC3339))
	require.Equal(t, "week-fable", windows[2].ID)
	require.Equal(t, 48.5, windows[2].UsedPercent)

	require.Empty(t, parseUsageWindows("You are on API billing.", currentUsageNow(t)))
	withoutReset := parseUsageWindows("Current session: 12% used\n", currentUsageNow(t))
	require.Len(t, withoutReset, 1)
	require.True(t, withoutReset[0].ResetsAt.IsZero())
}

func TestUsageWindowIDAndResetCurrentGrammar(t *testing.T) {
	require.Equal(t, "week-all-models", usageWindowID("week (all models)"))
	require.Equal(t, "week-fable", usageWindowID(" Week (Fable) "))

	now := currentUsageNow(t)
	for _, test := range []struct {
		text string
		want string
	}{
		{"Jul 9, 1:40pm (Australia/Brisbane)", "2026-07-09T13:40:00+10:00"},
		{"Jul 11, 6am (Australia/Brisbane)", "2026-07-11T06:00:00+10:00"},
		{"Jan 2, 6am (Australia/Brisbane)", "2027-01-02T06:00:00+10:00"},
		{"Jul 9, 1:40pm", "2026-07-09T13:40:00+10:00"},
	} {
		require.Equal(t, test.want, parseUsageReset(test.text, now).Format(time.RFC3339), test.text)
	}
	require.True(t, parseUsageReset("soon", now).IsZero())
	require.True(t, parseUsageReset("Jul 9, 1:40pm (Mars/Olympus)", now).IsZero())
}

func TestParseUsageOutput(t *testing.T) {
	now := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	limits, err := parseUsageOutput([]byte(`{"is_error":false,"result":"Current session: 42% used · resets Jul 9, 1:40pm (UTC)"}`), now)
	require.NoError(t, err)
	require.Len(t, limits.Windows, 1)
	require.Equal(t, "session", limits.Windows[0].ID)
	require.Equal(t, 42.0, limits.Windows[0].UsedPercent)
	require.False(t, limits.Windows[0].ResetsAt.IsZero())
}

func TestParseUsageOutputRejectsInvalidAndProviderError(t *testing.T) {
	_, err := parseUsageOutput([]byte(`{`), time.Now())
	require.Error(t, err)
	_, err = parseUsageOutput([]byte(`{"is_error":true,"result":"denied"}`), time.Now())
	require.ErrorContains(t, err, "denied")
}

func TestParseUsageOutputCurrentFailureBounds(t *testing.T) {
	_, err := parseUsageOutput([]byte(`{"is_error":true,"result":"   "}`), currentUsageNow(t))
	require.ErrorContains(t, err, "empty result")

	longResult := string(make([]byte, 300))
	payload, marshalErr := json.Marshal(map[string]any{"is_error": true, "result": longResult})
	require.NoError(t, marshalErr)
	_, err = parseUsageOutput(payload, currentUsageNow(t))
	require.LessOrEqual(t, len(err.Error()), len("claude /usage failed: ")+200)
}

func TestQueryRateLimitsOrdinaryBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	payload, err := json.Marshal(map[string]any{"type": "result", "result": currentUsagePanelSample})
	require.NoError(t, err)
	cli := writeShellScript(t, filepath.Join(dir, "claude"), "#!/bin/sh\ncat <<'EOF'\n"+string(payload)+"\nEOF\n")

	priorNow := usageNow
	usageNow = func() time.Time { return currentUsageNow(t) }
	t.Cleanup(func() { usageNow = priorNow })

	limits, err := QueryRateLimits(context.Background(), Options{
		CLIPath: cli, Cwd: dir, OrdinaryEnvironment: OrdinaryEnvironment(),
	})
	require.NoError(t, err)
	require.Len(t, limits.Windows, 3)
	require.Equal(t, "session", limits.Windows[0].ID)
}

func TestQueryRateLimitsOrdinaryFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	options := Options{Cwd: dir, OrdinaryEnvironment: OrdinaryEnvironment()}

	options.CLIPath = filepath.Join(dir, "missing")
	_, err := QueryRateLimits(t.Context(), options)
	require.ErrorContains(t, err, "run claude /usage")

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "failure"), "#!/bin/sh\nexit 17\n")
	_, err = QueryRateLimits(t.Context(), options)
	require.ErrorContains(t, err, "claude /usage exited 17")
}
