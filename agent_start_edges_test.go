package claudeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestStartSessionEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("12121212-1212-4212-8212-121212121212")

	homeFile := filepath.Join(t.TempDir(), "not-dir")
	require.NoError(t, os.WriteFile(homeFile, []byte("x"), 0o600))
	_, err := NewAgent(WithHome(homeFile)).startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "not a directory")

	materializeErrStore := &faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}
	materializeErrAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(materializeErrStore))
	_, err = materializeErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, ResumeID: string(sessionID)})
	require.ErrorContains(t, err, "load failed")

	modelConfigErr, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithEnv(map[string]string{envClaudeModelConfig: "[]"}))
	_, err = modelConfigErr.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, envClaudeModelConfig)

	settingsErrTransport := newFakeClaudeTransport()
	settingsErrTransport.controlErr = map[string]error{"get_settings": errors.New("settings failed")}
	settingsErrAgent, _, _ := newFakeLifecycleAgent(t, settingsErrTransport)
	session, err := settingsErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.False(t, session.fastModeKnown)
	require.NoError(t, session.Close(ctx))

	allowlistTransport := newFakeClaudeTransport()
	allowlistAgent, _, _ := newFakeLifecycleAgent(t, allowlistTransport, WithEnv(map[string]string{
		envClaudeModelConfig: `{"availableModels":["opus"]}`,
	}))
	session, err = allowlistAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Len(t, session.availableModels, 2)
	require.Equal(t, modelDefault, session.availableModels[0].Value)
	require.Equal(t, "opus", session.availableModels[1].Value)
	require.NoError(t, session.Close(ctx))

	settingsCwd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(settingsCwd, settingsDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(settingsCwd, settingsDirName, settingsFileName), []byte(`{
		"permissions": {"defaultMode": "acceptEdits"},
		"effortLevel": "medium"
	}`), 0o600))
	discoveredAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	session, err = discoveredAgent.startSession(ctx, sessionID, sessionStart{Cwd: settingsCwd})
	require.NoError(t, err)
	require.Equal(t, modeAcceptEdits, session.mode)
	require.NoError(t, session.Close(ctx))

	metaAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	session, err = metaAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{PermissionMode: string(modePlan)}})
	require.NoError(t, err)
	require.Equal(t, modePlan, session.mode)
	require.NoError(t, session.Close(ctx))

	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")
	osGeteuid = func() int { return 0 }
	require.NoError(t, os.Unsetenv("IS_SANDBOX"))
	t.Cleanup(func() {
		osGeteuid = previousGeteuid
		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})
	bypassAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithClaudeDefaultPermissionMode(permissionModeBypassPermissions))
	session, err = bypassAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, modeDefault, session.mode)
	require.NoError(t, session.Close(ctx))
	osGeteuid = previousGeteuid
	if hadSandbox {
		require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
	} else {
		require.NoError(t, os.Unsetenv("IS_SANDBOX"))
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	permissionErrAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	_, err = permissionErrAgent.startSession(cancelled, sessionID, sessionStart{Cwd: cwd})
	require.ErrorIs(t, err, context.Canceled)

	setModelErrTransport := newFakeClaudeTransport()
	setModelErrTransport.controlErr = map[string]error{"set_model": errors.New("set model failed")}
	setModelErrAgent, _, _ := newFakeLifecycleAgent(t, setModelErrTransport, WithEnv(map[string]string{envAnthropicModel: "opus"}))
	_, err = setModelErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "set model failed")

	reconcileTransport := newFakeClaudeTransport()
	reconcileTransport.settings = map[string]any{
		"applied":   map[string]any{"model": "sonnet", "effort": "medium"},
		"effective": map[string]any{"fastMode": true},
	}
	reconcileAgent, _, _ := newFakeLifecycleAgent(t, reconcileTransport)
	session, err = reconcileAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, effortHigh, session.effort)
	require.NoError(t, session.Close(ctx))

	modeClampTransport := newFakeClaudeTransport()
	modeClampTransport.initialize = map[string]any{
		"models": []any{
			map[string]any{"value": "opus", "displayName": "Opus"},
		},
	}
	modeClampAgent, _, _ := newFakeLifecycleAgent(t, modeClampTransport, WithClaudeDefaultPermissionMode("auto"))
	session, err = modeClampAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{Model: "opus"}})
	require.NoError(t, err)
	require.Equal(t, modeDefault, session.mode)
	require.NoError(t, session.Close(ctx))

	modeApplyErrTransport := newFakeClaudeTransport()
	modeApplyErrTransport.initialize = modeClampTransport.initialize
	modeApplyErrTransport.controlErr = map[string]error{"set_permission_mode": errors.New("set default mode failed")}
	modeApplyErrAgent, _, _ := newFakeLifecycleAgent(t, modeApplyErrTransport, WithClaudeDefaultPermissionMode("auto"))
	_, err = modeApplyErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{Model: "opus"}})
	require.ErrorContains(t, err, "set default mode failed")
}
