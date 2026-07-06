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

func TestAgentModelMetaAndOptions(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{
			Value:                 "claude-sonnet-4-5",
			DisplayName:           "Sonnet",
			Description:           "balanced",
			SupportedEffortLevels: []string{effortLow, effortHigh, effortHigh, ""},
			SupportsAutoMode:      true,
		},
		{
			Value:       "claude-opus-4-5-1m",
			DisplayName: "Opus 1m",
			Description: "1 million token context",
		},
		{Value: "claude-opus-4-5-1m", DisplayName: "Duplicate Opus"},
		{Value: ""},
	}
	session := &agentSession{
		model:                 "claude-sonnet-4-5",
		availableModels:       available,
		outputStyle:           "default",
		availableOutputStyles: []string{"default", "", "concise", "concise"},
		mode:                  modeAuto,
		effort:                effortHigh,
		fastMode:              true,
		fastModeKnown:         true,
	}

	meta := sessionResponseMeta(session)
	claudeMeta, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "claude-sonnet-4-5", claudeMeta[claudeModelMetaModelIDKey])
	require.Equal(t, effortHigh, claudeMeta[claudeModelMetaVariantKey])
	require.Equal(t, []string{effortLow, effortHigh}, claudeMeta[claudeModelMetaAvailableVariantsKey])
	require.Nil(t, claudeModelVariantMeta("", available, effortLow))

	infoMeta, ok := claudeModelInfoMeta(available[0])[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []string{effortLow, effortHigh}, infoMeta[claudeModelMetaSupportedEffortKey])
	require.Equal(t, true, infoMeta[claudeModelMetaSupportsAutoModeKey])
	require.Equal(t, defaultContextWindow, infoMeta[claudeModelMetaContextWindowKey])
	require.Nil(t, claudeModelInfoMeta(claude.AvailableModelInfo{Value: "unknown"}))
	require.Equal(t, largeContextWindow, modelContextWindowHint(available[1]))
	require.Equal(t, largeContextWindow, modelContextWindowHint(claude.AvailableModelInfo{Description: "has 1m context"}))
	require.Equal(t, largeContextWindow, modelContextWindowHint(claude.AvailableModelInfo{Value: modelTokenOpus}))
	require.Equal(t, defaultContextWindow, contextWindowForAvailableModel("claude-sonnet-4-5", nil))
	require.Equal(t, defaultContextWindow, contextWindowForAvailableModel("custom-sonnet", []claude.AvailableModelInfo{{Value: "custom-sonnet", Description: "unknown"}}))
	require.Equal(t, contextWindowForModel("custom-opus"), contextWindowForAvailableModel("custom-opus", []claude.AvailableModelInfo{{Value: "custom-opus", Description: "unknown"}}))
	require.Equal(t, claudeModelFamilyHaiku, modelFamily(claude.AvailableModelInfo{Value: "claude-haiku"}))
	require.Equal(t, claudeModelFamilyOpus, modelFamily(claude.AvailableModelInfo{DisplayName: "Opus"}))
	require.Equal(t, "", modelFamily(claude.AvailableModelInfo{Value: "custom"}))
	require.Equal(t, "value display description", modelHintText(claude.AvailableModelInfo{Value: "Value", DisplayName: "Display", Description: "Description"}))
	_, ok = availableModelInfo("missing", available)
	require.False(t, ok)
	require.Equal(t, []string{"a", "b"}, nonEmptyModelStrings([]string{"", "a", "a", "b"}))
	require.Equal(t, acp.SessionConfigOptionCategoryModel, *configCategory(acp.SessionConfigOptionCategoryModel))

	options := configOptions(modeAuto, "claude-sonnet-4-5", available, "default", []string{"default", "concise"}, effortHigh, true, true)
	require.Len(t, options, 4)
	require.Equal(t, configModel, options[0].Select.Id)
	require.Equal(t, configMode, options[1].Select.Id)
	require.Equal(t, configOutputStyle, options[2].Select.Id)
	require.Equal(t, configEffort, options[3].Select.Id)

	unstable := unstableConfigOptions(modeAuto, "claude-sonnet-4-5", available, "default", []string{"default"}, effortHigh, true, true)
	require.Len(t, unstable, 4)
	require.Equal(t, configTypeSelect, unstable[0].Select.Type)

	require.Len(t, configSelectOptions("custom", available), 3)
	require.Len(t, configSelectOptions("claude-sonnet-4-5", available), 2)
	require.Len(t, outputStyleSelectOptions("verbose", []string{"default", "", "default"}), 2)
	require.Contains(t, modeSelectOptions("claude-sonnet-4-5", available), acp.SessionConfigSelectOption{Name: modeNameAuto, Value: acp.SessionConfigValueId(modeAuto)})
	require.Nil(t, effortSelectOptions("missing", available, effortHigh))
	require.Contains(t, effortSelectOptions("claude-sonnet-4-5", available, effortMedium), acp.SessionConfigSelectOption{Name: "Medium", Value: acp.SessionConfigValueId(effortMedium)})
	require.Equal(t, []string{effortLow, effortHigh, effortHigh, ""}, effortLevelsForModel("claude-sonnet-4-5", available))
	require.Equal(t, "Extra High", effortDisplayName(effortXHigh))
	require.Equal(t, "Low", effortDisplayName(effortLow))
	require.Equal(t, "Medium", effortDisplayName(effortMedium))
	require.Equal(t, "High", effortDisplayName(effortHigh))
	require.Equal(t, "Max", effortDisplayName(effortMax))
	require.Equal(t, "custom", effortDisplayName("custom"))
	require.Equal(t, "Sonnet", modelDisplayName(available[0]))
	require.Equal(t, "fallback", modelDisplayName(claude.AvailableModelInfo{Value: "fallback"}))
	require.Nil(t, stringPtrIfNotEmpty(""))
	require.Equal(t, "x", *stringPtrIfNotEmpty("x"))
}

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
		turn:                  make(chan struct{}, agent.maxConcurrentPrompts()),
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
