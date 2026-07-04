package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestRunPassesContractFlags(t *testing.T) {
	originalServe := serve
	originalAgentVersion := agentVersion
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
	})

	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...claudeacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v1.2.3" }

	code := run(context.Background(), []string{
		"-path", "/bin/claude",
		"-home", "/tmp/claude",
		"-model", "sonnet",
		"-claude-bare",
		"-claude-permission-mode", "plan",
		"-claude-system-prompt", "system",
		"-claude-hide-auth",
	}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "v1.2.3", got.AgentVersion)
	require.Equal(t, "/bin/claude", got.ExecutablePath)
	require.Equal(t, "/tmp/claude", got.Home)
	require.Equal(t, "sonnet", got.DefaultModel)
	require.True(t, got.BareMode)
	require.Equal(t, "plan", got.DefaultPermissionMode)
	require.Equal(t, "system", got.DefaultSystemPrompt)
	require.True(t, got.HideAuth)
}

func TestRunVersion(t *testing.T) {
	originalAgentVersion := agentVersion
	t.Cleanup(func() { agentVersion = originalAgentVersion })
	agentVersion = func() string { return "v9.9.9" }

	var stdout bytes.Buffer
	code := run(context.Background(), []string{"-version"}, bytes.NewBuffer(nil), &stdout, bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "v9.9.9\n", stdout.String())
}
