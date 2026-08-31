package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestRunPassesCurrentFlags(t *testing.T) {
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })
	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, options ...claudeacp.Option) error {
		for _, option := range options {
			option(&got)
		}

		return nil
	}
	code := run(t.Context(), []string{"-path", "/bin/claude", "-home", "/tmp/home", "-scratch-dir", "/tmp/scratch", "-model", "sonnet", "-claude-bare"}, bytes.NewReader(nil), io.Discard, io.Discard)
	require.Zero(t, code)
	require.Equal(t, "/bin/claude", got.ExecutablePath)
	require.Equal(t, "/tmp/home", got.Home)
	require.Equal(t, "/tmp/scratch", got.ScratchDir)
	require.Equal(t, "sonnet", got.DefaultModel)
	require.True(t, got.BareMode)
}

func TestRunVersionAndUnknownFlag(t *testing.T) {
	originalVersion := agentVersion
	t.Cleanup(func() { agentVersion = originalVersion })
	agentVersion = func() string { return "test-version" }
	var output bytes.Buffer
	require.Zero(t, run(t.Context(), []string{"-version"}, bytes.NewReader(nil), &output, io.Discard))
	require.Equal(t, "test-version\n", output.String())
	require.Equal(t, 2, run(t.Context(), []string{"-removed-flag"}, bytes.NewReader(nil), io.Discard, io.Discard))
}
