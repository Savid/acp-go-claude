package claude

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
