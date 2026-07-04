package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadTranscriptJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("\n"+
		`{"type":"user","sessionId":"session-1","cwd":"/repo"}`+"\n"+
		`{"type":"assistant"}`+"\n"), 0o600))

	entries, sessionID, cwd, err := readTranscriptJSONL(path)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "session-1", sessionID)
	require.Equal(t, "/repo", cwd)
}
