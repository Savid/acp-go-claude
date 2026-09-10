package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestModelSelectionAndModeBranches(t *testing.T) {
	previousGeteuid := osGeteuid
	osGeteuid = func() int { return 0 }
	t.Cleanup(func() { osGeteuid = previousGeteuid })

	available := []claude.AvailableModelInfo{
		{Value: "sonnet", DisplayName: "Sonnet", SupportedEffortLevels: []string{effortLow}, SupportsAutoMode: true},
		{Value: "opus", DisplayName: "Opus", SupportedEffortLevels: []string{effortHigh}},
	}

	require.False(t, bypassPermissionsAvailable(map[string]string{}))
	require.False(t, modeAvailableForModel(modeBypassPermissions, "sonnet", available, false))
	require.True(t, modeAvailableForModel(modeBypassPermissions, "sonnet", available, true))
	require.False(t, modeAvailableForModel("bad", "sonnet", available, true))
	require.True(t, modeAvailableForModel(modeAuto, "sonnet", available, false))
	require.False(t, modeAvailableForModel(modeAuto, "opus", available, false))
	require.True(t, modelSupportsAutoMode("sonnet", available))
	require.False(t, modelSupportsAutoMode("missing", available))
	require.True(t, bypassPermissionsAvailable(map[string]string{"IS_SANDBOX": "1"}))

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
	require.Equal(t, "Opus", model)
	require.Equal(t, "Opus", cliModel)
	model, cliModel = session.modelSelection("opus")
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

func TestResolvedModelCapabilityLookupPreservesExactRestrictions(t *testing.T) {
	t.Parallel()
	const modelID = "claude-sonnet-5"
	alias := claude.AvailableModelInfo{
		Value: "sonnet", ResolvedModel: modelID, SupportsAutoMode: true,
		SupportedEffortLevels: []string{effortLow, effortHigh},
	}
	for _, test := range []struct {
		name      string
		selection string
		models    []claude.AvailableModelInfo
		wantAuto  bool
		want      []string
	}{
		{"exact resolved identity", modelID, []claude.AvailableModelInfo{alias}, true, alias.SupportedEffortLevels},
		{"different version", "claude-sonnet-4-6", []claude.AvailableModelInfo{alias}, false, nil},
		{"explicit row wins", modelID, []claude.AvailableModelInfo{alias, {Value: modelID, EffortUnsupported: true}}, false, nil},
		{"disabled alias", modelID, []claude.AvailableModelInfo{{Value: "sonnet", ResolvedModel: modelID, Disabled: true, SupportsAutoMode: true, SupportedEffortLevels: []string{effortHigh}}}, false, nil},
		{"explicit effort refusal", modelID, []claude.AvailableModelInfo{{Value: "sonnet", ResolvedModel: modelID, EffortUnsupported: true, SupportsAutoMode: true, SupportedEffortLevels: []string{effortHigh}}}, true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.wantAuto, modelSupportsAutoMode(test.selection, test.models))
			levels := effortLevelsForModel(test.selection, test.models)
			require.Equal(t, test.want, levels)
			if len(levels) > 0 {
				levels[0] = "changed"
				require.Equal(t, effortLow, alias.SupportedEffortLevels[0], "lookup results must own their slices")
			}
		})
	}
}

// TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess proves the
// rule that decides whether the session may offer a mode that skips permission
// prompts entirely. An unprivileged agent may offer it. A root agent may offer
// it only when the environment explicitly declares a sandbox, because bypassing
// prompts as root means unreviewed tool calls run with full privilege.
func TestBypassPermissionsAvailabilityFollowsThePrivilegeOfTheProcess(t *testing.T) {
	previousGeteuid := osGeteuid
	t.Cleanup(func() { osGeteuid = previousGeteuid })

	osGeteuid = func() int { return 1000 }
	require.True(t, bypassPermissionsAvailable(nil), "an unprivileged agent may offer bypass mode")

	osGeteuid = func() int { return 0 }
	require.False(t, bypassPermissionsAvailable(nil), "a root agent offered bypass mode outside a sandbox")
	require.False(t, bypassPermissionsAvailable(map[string]string{"IS_SANDBOX": " "}), "a blank marker declared a sandbox")

	require.True(t, bypassPermissionsAvailable(map[string]string{"IS_SANDBOX": "1"}), "a declared sandbox did not re-enable bypass mode")

	// The marker is judged in the environment the native process inherits, so a
	// host's ambient block decides it, not the adapter's own process.
	t.Setenv("IS_SANDBOX", "")
	sandboxed := NewAgent(WithAmbientEnvironment(map[string]string{"HOME": "/host/home", "IS_SANDBOX": "1"}))
	require.True(t, bypassPermissionsAvailable(sandboxed.effectiveNativeEnvironment(nil)))
	t.Setenv("IS_SANDBOX", "1")
	unsandboxed := NewAgent(WithAmbientEnvironment(map[string]string{"HOME": "/host/home"}))
	require.False(t, bypassPermissionsAvailable(unsandboxed.effectiveNativeEnvironment(nil)))
	require.True(t, bypassPermissionsAvailable(unsandboxed.effectiveNativeEnvironment(map[string]string{"IS_SANDBOX": "1"})),
		"a session overlay declaring the sandbox re-enables bypass mode")
}

// TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable proves the advertised
// mode list is derived from that same rule rather than being fixed. A client
// that never sees the option cannot select it, so this is where the privilege
// rule actually reaches the protocol.
func TestModeSelectOptionsOfferBypassOnlyWhenItIsAvailable(t *testing.T) {
	available := []claude.AvailableModelInfo{{Value: "opus", DisplayName: "Opus"}}
	bypass := acp.SessionConfigSelectOption{
		Name:  "Bypass Permissions",
		Value: acp.SessionConfigValueId(modeBypassPermissions),
	}

	require.NotContains(t, modeSelectOptions("opus", available, false), bypass)

	offered := modeSelectOptions("opus", available, true)
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
