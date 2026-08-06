package claudeacp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSetSessionConfigValueEdgeBranches(t *testing.T) {
	ctx := context.Background()
	available := []claude.AvailableModelInfo{
		{Value: "sonnet", SupportedEffortLevels: []string{effortLow}, SupportsAutoMode: true},
		{Value: "opus", SupportedEffortLevels: []string{effortHigh}},
	}

	t.Run("model clamp permission mode failure", func(t *testing.T) {
		agent, transport, cleanup := newConfigEdgeSession(t, available)
		defer cleanup()
		transport.controlErr = map[string]error{"set_permission_mode": errors.New("mode failed")}
		_, err := agent.SetSessionConfigOption(ctx, SetModelRequest("session-1", "opus"))
		require.ErrorContains(t, err, "mode failed")
	})

	t.Run("model clamp effort failure", func(t *testing.T) {
		agent, transport, cleanup := newConfigEdgeSession(t, available)
		defer cleanup()
		session := agent.sessions["session-1"]
		session.mode = modeDefault
		session.effort = effortMedium
		transport.controlErr = map[string]error{"apply_flag_settings": errors.New("effort failed")}
		_, err := agent.SetSessionConfigOption(ctx, SetModelRequest("session-1", "sonnet"))
		require.ErrorContains(t, err, "effort failed")
	})

	t.Run("acquire turn failure", func(t *testing.T) {
		agent, _, cleanup := newConfigEdgeSession(t, available)
		defer cleanup()
		session := agent.sessions["session-1"]
		session.turn <- struct{}{}
		_, err := agent.SetSessionConfigOption(ctx, SetModelRequest("session-1", "opus"))
		require.Error(t, err)
	})

	t.Run("poison after acquire turn", func(t *testing.T) {
		agent, _, cleanup := newConfigEdgeSession(t, available)
		defer cleanup()
		session := agent.sessions["session-1"]
		session.turnAcquiredHook = func(int) {
			session.mu.Lock()
			session.poisonCause = "poisoned after config acquire"
			session.mu.Unlock()
		}
		_, err := agent.SetSessionConfigOption(ctx, SetModelRequest("session-1", "opus"))
		require.ErrorContains(t, err, "poisoned after config acquire")
	})

	t.Run("emit config update failure", func(t *testing.T) {
		agent, _, cleanup := newConfigEdgeSession(t, available)
		defer cleanup()
		conn, ok := agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("emit failed")
		_, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest("session-1", configMode, acp.SessionConfigValueId(modeDefault)))
		require.ErrorContains(t, err, "emit failed")
	})
}

func newConfigEdgeSession(t *testing.T, available []claude.AvailableModelInfo) (*Agent, *fakeClaudeTransport, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport)
	client := claude.NewClient(agent.log, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))
	session := &agentSession{
		agent:                 agent,
		id:                    "session-1",
		cwd:                   t.TempDir(),
		model:                 "sonnet",
		availableModels:       append([]claude.AvailableModelInfo(nil), available...),
		mode:                  modeAuto,
		effort:                effortHigh,
		outputStyle:           "default",
		availableOutputStyles: []string{"default"},
		client:                client,
		turn:                  make(chan struct{}, sessionTurnCapacity),
	}
	agent.sessions[session.id] = session

	return agent, transport, func() { _ = client.Close() }
}

func TestModelSelectionAndModeBranches(t *testing.T) {
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

	available := []claude.AvailableModelInfo{
		{Value: "sonnet", DisplayName: "Sonnet", SupportedEffortLevels: []string{effortLow}, SupportsAutoMode: true},
		{Value: "opus", DisplayName: "Opus", SupportedEffortLevels: []string{effortHigh}},
	}

	require.False(t, bypassPermissionsAvailable())
	require.False(t, modeAvailableForModel(modeBypassPermissions, "sonnet", available))
	require.False(t, modeAvailableForModel("bad", "sonnet", available))
	require.True(t, modeAvailableForModel(modeAuto, "sonnet", available))
	require.False(t, modeAvailableForModel(modeAuto, "opus", available))
	require.True(t, modelSupportsAutoMode("sonnet", available))
	require.False(t, modelSupportsAutoMode("missing", available))
	require.NoError(t, os.Setenv("IS_SANDBOX", "1"))
	require.True(t, bypassPermissionsAvailable())

	require.Equal(t, initialModelSelection{Model: "sonnet", ShouldApply: false}, selectInitialModel("sonnet", "", "", available))
	require.Equal(t, initialModelSelection{Model: "opus", ShouldApply: true}, selectInitialModel("", "opus", "", available))
	require.Equal(t, initialModelSelection{Model: "opus", ShouldApply: false}, selectInitialModel("", "", "opus", available))
	require.Equal(t, initialModelSelection{Model: "custom", ShouldApply: true}, selectInitialModel("", "custom", "", available))
	require.Equal(t, initialModelSelection{Model: "sonnet", ShouldApply: true}, selectInitialModel("", "", "", available))
	require.Equal(t, initialModelSelection{}, selectInitialModel("", "", "", nil))

	require.Equal(t, modePlan, acpModeForPermission(string(modePlan)))
	require.Equal(t, modeAcceptEdits, acpModeForPermission(permissionModeAcceptEdits))
	require.Equal(t, modeBypassPermissions, acpModeForPermission(permissionModeBypassPermissions))
	require.Equal(t, modeAuto, acpModeForPermission(string(modeAuto)))
	require.Equal(t, modeDontAsk, acpModeForPermission(permissionModeDontAsk))
	require.Equal(t, modeDefault, acpModeForPermission("bad"))

	for _, tc := range []struct {
		mode acp.SessionModeId
		want string
		ok   bool
	}{
		{modeDefault, string(modeDefault), true},
		{modePlan, string(modePlan), true},
		{modeAcceptEdits, permissionModeAcceptEdits, true},
		{modeBypassPermissions, permissionModeBypassPermissions, true},
		{modeAuto, "auto", true},
		{modeDontAsk, permissionModeDontAsk, true},
		{"bad", "", false},
	} {
		got, ok := permissionModeForACP(tc.mode)
		require.Equal(t, tc.want, got)
		require.Equal(t, tc.ok, ok)
	}

	require.Equal(t, acp.PositionEncodingKindUtf8, selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16, acp.PositionEncodingKindUtf8}))
	require.Equal(t, acp.PositionEncodingKindUtf16, selectPositionEncoding([]acp.PositionEncodingKind{"bad", acp.PositionEncodingKindUtf16}))
	require.Equal(t, acp.PositionEncodingKindUtf16, selectPositionEncoding([]acp.PositionEncodingKind{"bad", acp.PositionEncodingKindUtf32}))
	require.Equal(t, acp.PositionEncodingKindUtf16, selectPositionEncoding(nil))

	session := &agentSession{
		model:           "sonnet",
		modelOverrides:  map[string]string{"opus": "claude-opus-real"},
		availableModels: available,
		mode:            modeAuto,
		effort:          effortHigh,
	}
	model, cliModel := session.modelSelection("Opus")
	require.Equal(t, "opus", model)
	require.Equal(t, "claude-opus-real", cliModel)

	modeChanged, mode, effortChanged, effort := session.setModelAndClampMode("opus")
	require.True(t, modeChanged)
	require.Equal(t, modeDefault, mode)
	require.False(t, effortChanged)
	require.Equal(t, effortHigh, effort)

	session.effort = effortMedium
	modeChanged, _, effortChanged, effort = session.setModelAndClampMode("sonnet")
	require.False(t, modeChanged)
	require.True(t, effortChanged)
	require.Equal(t, effortLow, effort)

	require.Equal(t, []claude.SlashCommand(nil), session.commands())
	session.availableCommands = []claude.SlashCommand{{Name: "help"}}
	commands := session.commands()
	commands[0].Name = "changed"
	require.Equal(t, "help", session.availableCommands[0].Name)
}

func TestReconcileEffortForModel(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{Value: "a", SupportedEffortLevels: []string{effortLow, effortHigh}},
		{Value: "b", SupportedEffortLevels: []string{effortLow, effortXHigh}},
		{Value: "c", SupportedEffortLevels: []string{effortLow}},
	}
	require.Equal(t, "", func() string {
		got, changed := reconcileEffortForModel("a", available, "")
		require.False(t, changed)

		return got
	}())
	got, changed := reconcileEffortForModel("a", available, effortLow)
	require.Equal(t, effortLow, got)
	require.False(t, changed)
	got, changed = reconcileEffortForModel("a", available, effortMedium)
	require.Equal(t, effortHigh, got)
	require.True(t, changed)
	got, changed = reconcileEffortForModel("b", available, effortMedium)
	require.Equal(t, effortXHigh, got)
	require.True(t, changed)
	got, changed = reconcileEffortForModel("c", available, effortMedium)
	require.Equal(t, effortLow, got)
	require.True(t, changed)
	got, changed = reconcileEffortForModel("missing", available, effortMedium)
	require.Equal(t, "", got)
	require.True(t, changed)
}

// TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess proves the
// rule that decides whether the session may offer a mode that skips permission
// prompts entirely. An unprivileged agent may offer it. A root agent may offer
// it only when the environment explicitly declares a sandbox, because bypassing
// prompts as root means unreviewed tool calls run with full privilege.
func TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess(t *testing.T) {
	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")

	t.Cleanup(func() {
		osGeteuid = previousGeteuid

		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})

	require.NoError(t, os.Unsetenv("IS_SANDBOX"))

	osGeteuid = func() int { return 1000 }
	require.True(t, bypassPermissionsAvailable(), "an unprivileged agent may offer bypass mode")

	osGeteuid = func() int { return 0 }
	require.False(t, bypassPermissionsAvailable(), "a root agent offered bypass mode outside a sandbox")

	require.NoError(t, os.Setenv("IS_SANDBOX", "1"))
	require.True(t, bypassPermissionsAvailable(), "a declared sandbox did not re-enable bypass mode")
}

// TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable proves the advertised
// mode list is derived from that same rule rather than being fixed. A client
// that never sees the option cannot select it, so this is where the privilege
// rule actually reaches the protocol.
func TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable(t *testing.T) {
	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")

	t.Cleanup(func() {
		osGeteuid = previousGeteuid

		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})

	require.NoError(t, os.Unsetenv("IS_SANDBOX"))

	available := []claude.AvailableModelInfo{{Value: "opus", DisplayName: "Opus"}}
	bypass := acp.SessionConfigSelectOption{
		Name:  "Bypass Permissions",
		Value: acp.SessionConfigValueId(modeBypassPermissions),
	}

	osGeteuid = func() int { return 0 }
	require.NotContains(t, modeSelectOptions("opus", available), bypass)

	osGeteuid = func() int { return 1000 }

	offered := modeSelectOptions("opus", available)
	require.Contains(t, offered, bypass)
	require.Contains(t, offered, acp.SessionConfigSelectOption{
		Name: modeNameDefault, Value: acp.SessionConfigValueId(modeDefault),
	})
	require.Contains(t, offered, acp.SessionConfigSelectOption{
		Name: modeNameDontAsk, Value: acp.SessionConfigValueId(modeDontAsk),
	})
}

// TestProviderAuthSettingsWithoutCredentialsAreNotConfigured proves a settings
// file that parses but carries neither an apiKeyHelper nor any credential
// environment variable does not count as provider auth. Counting it would make
// the wrapper treat an unrelated Claude settings file as an existing
// credential and suppress its own brokering.
func TestProviderAuthSettingsWithoutCredentialsAreNotConfigured(t *testing.T) {
	require.False(t, providerAuthSettingsContentConfigured([]byte(`{}`)))
	require.False(t, providerAuthSettingsContentConfigured([]byte(`{"env":{"EDITOR":"vi"}}`)))
	require.False(
		t,
		providerAuthSettingsContentConfigured([]byte(`{"env":{"`+providerAuthEnvAnthropicAPIKey+`":"  "}}`)),
		"a blank credential value counted as configured",
	)

	require.True(
		t,
		providerAuthSettingsContentConfigured([]byte(`{"env":{"`+providerAuthEnvAnthropicAPIKey+`":"secret"}}`)),
	)
}
