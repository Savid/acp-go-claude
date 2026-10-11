package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTranscriptStartedReadsOnlyUntilARecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, test := range map[string]struct {
		content string
		want    bool
	}{
		"empty":        {"", false},
		"blank":        {strings.Repeat(" \n", 5<<10), false},
		"record":       {"{\"a\":1}\n", true},
		"blank prefix": {strings.Repeat("\n", 5<<10) + "{\"a\":1}\n", true},
		"unterminated": {"{\"a\":", true},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(test.content), 0o600))

		started, err := TranscriptStarted(path)
		require.NoError(t, err, name)
		require.Equal(t, test.want, started, name)
	}

	started, err := TranscriptStarted(filepath.Join(dir, "absent.jsonl"))
	require.NoError(t, err, "an absent transcript is an empty conversation, not a failure")
	require.False(t, started)
}

func TestValidateRowsRequiresThisSessionsIdentity(t *testing.T) {
	t.Parallel()

	owned := [][]byte{[]byte(`{"type":"user","sessionId":"s"}`), []byte(`{"type":"assistant"}`)}
	require.NoError(t, ValidateRows(owned, "s"))

	require.ErrorContains(t, ValidateRows(owned, "other"), "session id mismatch")
	require.ErrorContains(t, ValidateRows([][]byte{[]byte(`{"type":"user"}`)}, "s"), "no session identity")
	require.Error(t, ValidateRows([][]byte{[]byte(`{"type":7,"sessionId":"s"}`)}, "s"))
	require.Error(t, ValidateRows([][]byte{[]byte(`{"type":"user","sessionId":"s","message":7}`)}, "s"),
		"a member the replay decoder reads must be typed here too")
	require.Error(t, ValidateRows([][]byte{[]byte(`{`)}, "s"))
}
