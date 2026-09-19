package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadRowsRefusesMalformedTranscripts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	missing, err := ReadRows(filepath.Join(dir, "absent.jsonl"))
	require.NoError(t, err, "an absent transcript is an empty conversation, not a failure")
	require.Nil(t, missing)

	valid := filepath.Join(dir, "valid.jsonl")
	require.NoError(t, os.WriteFile(valid, []byte("{\"a\":1}\n{\"b\":2}\n"), 0o600))
	rows, err := ReadRows(valid)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	torn := filepath.Join(dir, "torn.jsonl")
	require.NoError(t, os.WriteFile(torn, []byte("{\"a\":1}\n{\"b\":"), 0o600))
	_, err = ReadRows(torn)
	require.ErrorContains(t, err, "invalid native transcript row")

	oversize := filepath.Join(dir, "oversize.jsonl")
	require.NoError(t, os.WriteFile(oversize, []byte(`{"a":"`+strings.Repeat("x", 17<<20)+`"}`+"\n"), 0o600))
	_, err = ReadRows(oversize)
	require.Error(t, err, "a row beyond the scanner bound is refused rather than truncated")
}

func TestWriteRowsReplacesTheTranscript(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "projects", "p", "s.jsonl")
	require.NoError(t, WriteRows(path, [][]byte{[]byte(`{"a":1}`)}))
	require.NoError(t, WriteRows(path, [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)}))

	rows, err := ReadRows(path)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".transcript-*"))
	require.NoError(t, err)
	require.Empty(t, leftovers)
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
