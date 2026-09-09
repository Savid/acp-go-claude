package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type recordingModelCatalog struct {
	mu            sync.Mutex
	models        map[string][]claude.APIModel
	accesses      []claude.ModelCatalogAccess
	invalidations int
	closes        int
}

func (c *recordingModelCatalog) List(_ context.Context, access claude.ModelCatalogAccess) ([]claude.APIModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.accesses = append(c.accesses, access)
	models, ok := c.models[access.Credential]
	if !ok {
		return nil, errors.New("unrecognized synthetic catalog credential")
	}

	result := slices.Clone(models)
	for i := range result {
		result[i].SupportedEffortLevels = slices.Clone(result[i].SupportedEffortLevels)
	}

	return result, nil
}

func (c *recordingModelCatalog) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidations++
}

func (c *recordingModelCatalog) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
}

func (c *recordingModelCatalog) snapshot() ([]claude.ModelCatalogAccess, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.accesses), c.invalidations, c.closes
}

type modelCatalogTransport struct {
	*fakeClaudeTransport
	environment map[string]string
}

func (t *modelCatalogTransport) LaunchEnvironment() map[string]string {
	return maps.Clone(t.environment)
}

type modelCatalogHarness struct {
	agent    *Agent
	catalog  *recordingModelCatalog
	launches []*modelCatalogTransport
}

func newModelCatalogHarness(t *testing.T, models map[string][]claude.APIModel, options ...Option) *modelCatalogHarness {
	t.Helper()

	agent := newAuthAgent(t, options...)
	if agent.modelCatalog != nil {
		agent.modelCatalog.Close()
	}

	harness := &modelCatalogHarness{
		agent:   agent,
		catalog: &recordingModelCatalog{models: models},
	}
	agent.modelCatalog = harness.catalog
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		// Capture only what this test's native launch actually receives. A
		// broker callback cannot change an already constructed transport.
		transport := &modelCatalogTransport{
			fakeClaudeTransport: newFakeClaudeTransport(),
			environment:         maps.Clone(options.Env),
		}
		harness.launches = append(harness.launches, transport)

		return claude.NewClient(log, options, transport)
	}
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	return harness
}

func catalogModelOption(t *testing.T, options []acp.SessionConfigOption) *acp.SessionConfigOptionSelect {
	t.Helper()

	for _, option := range options {
		if option.Select != nil && option.Select.Id == configModel {
			return option.Select
		}
	}

	t.Fatal("session did not advertise a model configuration")

	return nil
}

func catalogModelValues(t *testing.T, options []acp.SessionConfigOption) []string {
	t.Helper()
	model := catalogModelOption(t, options)
	require.NotNil(t, model.Options.Ungrouped)
	values := make([]string, 0, len(*model.Options.Ungrouped))
	for _, option := range *model.Options.Ungrouped {
		values = append(values, string(option.Value))
	}

	return values
}

func TestModelCatalogLateProviderAuthUsesNewNativeLaunch(t *testing.T) {
	seams := newAuthSeams(t)
	const key = "synthetic-late-api-key"
	const modelID = "claude-fable-5-1"
	h := newModelCatalogHarness(t, map[string][]claude.APIModel{
		key: {{ID: modelID, DisplayName: "Claude Fable 5.1", ContextWindow: 1000000, MaxOutputTokens: 64000}},
	})
	agent := h.agent
	require.NotNil(t, agent.providerAuth)
	accesses, invalidations, closes := h.catalog.snapshot()
	require.Empty(t, accesses)
	require.Zero(t, invalidations)
	require.Zero(t, closes)

	cwd := t.TempDir()
	initial, err := agent.NewSession(t.Context(), NewSessionRequest(cwd))
	require.NoError(t, err)
	original := agent.sessions[initial.SessionId]
	require.NotContains(t, catalogModelValues(t, initial.ConfigOptions), modelID)
	require.Empty(t, h.launches[0].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])
	accesses, _, _ = h.catalog.snapshot()
	require.Empty(t, accesses, "a logged-out launch must not consult the account catalog")

	broker := agent.providerAuth
	generation := authCatalogGeneration(t, broker, initial.SessionId)
	params := authorizeParams(initial.SessionId, generation)
	params[authFieldMethod] = authMethodAPIKey
	authorized, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)
	flow, ok := authorized.(authAuthorizeResult)
	require.True(t, ok)
	accesses, afterAuthorize, _ := h.catalog.snapshot()
	require.Greater(t, afterAuthorize, invalidations, "new auth intent invalidates provider observations")
	require.Empty(t, accesses)

	_, err = broker.callback(t.Context(), authParams(t, map[string]any{
		authFieldSessionID: string(initial.SessionId), authFieldProviderID: authProviderID,
		authFieldMethod: authMethodAPIKey, authFieldFlowID: flow.FlowID, authFieldInput: key,
	}))
	require.NoError(t, err)
	accesses, afterCallback, _ := h.catalog.snapshot()
	require.Greater(t, afterCallback, afterAuthorize, "credential confirmation invalidates provider observations")
	require.Empty(t, accesses, "saving credentials does not authenticate the running native process")
	require.Equal(t, initial.ConfigOptions, sessionConfigOptions(original))
	require.Empty(t, h.launches[0].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])

	harvested, err := broker.credential(t.Context(), authParams(t, flowParams(string(initial.SessionId), flow.FlowID)))
	require.NoError(t, err)
	credential, ok := harvested.(authCredentialResult)
	require.True(t, ok)
	require.Equal(t, key, credential.Credential.API.Key)
	boundOptions := ClaudeOptions{ProviderAuth: map[string]ProviderAuthBinding{
		authProviderID: {
			ConnectionID: credential.ConnectionID, Revision: credential.Revision,
			BindingGeneration: credential.BindingGeneration, Credential: credential.Credential,
		},
	}}

	_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest(initial.SessionId, cwd, WithSessionMeta(boundOptions.Meta())))
	requireSessionResumeIncompatible(t, err, acpFieldSessionID)
	_, err = agent.LoadSession(t.Context(), LoadSessionRequest(initial.SessionId, cwd, WithSessionMeta(boundOptions.Meta())))
	requireSessionResumeIncompatible(t, err, acpFieldSessionID)
	require.Same(t, original, agent.sessions[initial.SessionId])
	require.Len(t, h.launches, 1)
	require.Zero(t, h.launches[0].CloseCalls())
	accesses, _, _ = h.catalog.snapshot()
	require.Empty(t, accesses)

	authenticated, err := agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(boundOptions.Meta())))
	require.NoError(t, err)
	require.Len(t, h.launches, 2)
	require.Same(t, h.catalog, agent.modelCatalog)
	require.Equal(t, key, h.launches[1].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])
	accesses, _, _ = h.catalog.snapshot()
	require.Equal(t, []claude.ModelCatalogAccess{{Endpoint: "https://api.anthropic.com/v1/models", Credential: key}}, accesses)
	require.Contains(t, catalogModelValues(t, authenticated.ConfigOptions), modelID)
	claudeMeta, ok := authenticated.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	authMeta, ok := claudeMeta[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, authInjectionApplied, authMeta["injection"])
	require.Equal(t, initial.ConfigOptions, sessionConfigOptions(original))
	require.Empty(t, original.configuration.Env[providerAuthEnvAnthropicAPIKey])

	reused, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(authenticated.SessionId, cwd, WithSessionMeta(boundOptions.Meta())))
	require.NoError(t, err)
	require.Equal(t, authenticated.ConfigOptions, reused.ConfigOptions)
	require.Len(t, h.launches, 2)
	afterReuse, _, _ := h.catalog.snapshot()
	require.Equal(t, accesses, afterReuse, "an unchanged live session keeps its process catalog")

	_, beforeDisconnect, _ := h.catalog.snapshot()
	_, err = broker.disconnect(t.Context(), authParams(t, map[string]any{
		authFieldSessionID: string(initial.SessionId), authFieldProviderID: authProviderID,
		authFieldConnectionID: credential.ConnectionID, authFieldBindingGeneration: credential.BindingGeneration,
	}))
	require.NoError(t, err)
	_, afterDisconnect, _ := h.catalog.snapshot()
	require.Greater(t, afterDisconnect, beforeDisconnect)
	_, err = agent.Logout(t.Context(), acp.LogoutRequest{})
	require.NoError(t, err)
	afterLogout, invalidations, closes := h.catalog.snapshot()
	require.Greater(t, invalidations, afterDisconnect)
	require.Equal(t, accesses, afterLogout)
	require.Zero(t, closes, "logout invalidates the shared service without closing it")
	require.Zero(t, seams.loginCalls)
	require.Zero(t, seams.statusCalls)
	require.Zero(t, seams.logoutCalls)

	require.NoError(t, agent.Close())
	require.NoError(t, agent.Close())
	_, _, closes = h.catalog.snapshot()
	require.Equal(t, 1, closes, "Agent.Close owns the catalog service exactly once")
}

func TestModelCatalogCarrierReplacementUsesReplacementCredential(t *testing.T) {
	for _, operation := range []string{"load", "resume"} {
		t.Run(operation, func(t *testing.T) {
			newAuthSeams(t)
			store := NewInMemorySessionStore()
			const oldKey, newKey = "synthetic-old-api-key", "synthetic-new-api-key"
			const oldModel, newModel = "claude-fable-old", "claude-fable-5-1"
			h := newModelCatalogHarness(t, map[string][]claude.APIModel{
				oldKey: {{ID: oldModel, DisplayName: "Earlier account model"}},
				newKey: {{ID: newModel, DisplayName: "Claude Fable 5.1"}},
			}, WithSessionStore(store))
			cwd := t.TempDir()
			oldOptions := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: oldKey}}
			newOptions := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: newKey}}
			created, err := h.agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(oldOptions.Meta())))
			require.NoError(t, err)
			require.Contains(t, catalogModelValues(t, created.ConfigOptions), oldModel)
			require.NotContains(t, catalogModelValues(t, created.ConfigOptions), newModel)
			original := h.agent.sessions[created.SessionId]
			require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(created.SessionId)},
				testStoredSessionEntries(t, oldOptions, []byte(`{"type":"user"}`))))

			var options []acp.SessionConfigOption
			if operation == "load" {
				response, loadErr := h.agent.LoadSession(t.Context(), LoadSessionRequest(created.SessionId, cwd, WithSessionMeta(newOptions.Meta())))
				require.NoError(t, loadErr)
				options = response.ConfigOptions
			} else {
				response, resumeErr := h.agent.ResumeSession(t.Context(), ResumeSessionRequest(created.SessionId, cwd, WithSessionMeta(newOptions.Meta())))
				require.NoError(t, resumeErr)
				options = response.ConfigOptions
			}

			require.NotSame(t, original, h.agent.sessions[created.SessionId])
			require.Len(t, h.launches, 2)
			require.Equal(t, 1, h.launches[0].CloseCalls())
			require.Equal(t, oldKey, h.launches[0].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])
			require.Equal(t, newKey, h.launches[1].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])
			require.Contains(t, catalogModelValues(t, options), newModel)
			require.NotContains(t, catalogModelValues(t, options), oldModel)
			accesses, _, closes := h.catalog.snapshot()
			require.Equal(t, []claude.ModelCatalogAccess{
				{Endpoint: "https://api.anthropic.com/v1/models", Credential: oldKey},
				{Endpoint: "https://api.anthropic.com/v1/models", Credential: newKey},
			}, accesses)
			require.Zero(t, closes, "replacing a native process preserves the Agent-owned service")
		})
	}
}

func TestModelCatalogSelectionRoundTripsExactIDs(t *testing.T) {
	newAuthSeams(t)
	const key = "synthetic-selection-api-key"
	const listedID = "claude-fable-5-1"
	h := newModelCatalogHarness(t, map[string][]claude.APIModel{
		key: {{ID: listedID, DisplayName: "Claude Fable 5.1"}},
	})
	options := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: key}}
	created, err := h.agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMeta(options.Meta())))
	require.NoError(t, err)
	require.Contains(t, catalogModelValues(t, created.ConfigOptions), listedID)

	for _, id := range []string{listedID, "claude-fable-5-1-20260901"} {
		response, err := h.agent.SetSessionConfigOption(t.Context(), SetModelRequest(created.SessionId, id))
		require.NoError(t, err)
		require.Equal(t, acp.SessionConfigValueId(id), catalogModelOption(t, response.ConfigOptions).CurrentValue)
		require.Contains(t, catalogModelValues(t, response.ConfigOptions), id)
		var sentModel any
		for _, sent := range h.launches[0].Sent() {
			if request, ok := sent.(claude.ControlRequest); ok && request.Request["subtype"] == "set_model" {
				sentModel = request.Request["model"]
			}
		}
		require.Equal(t, id, sentModel, "an explicit model ID must reach native selection unchanged")
	}
}

func TestModelCatalogNativeRelaunchRefreshesFactsAndConfiguration(t *testing.T) {
	newAuthSeams(t)
	const key = "synthetic-relaunch-api-key"
	const modelID = "claude-fable-5-1"
	const retiredID, addedID = "claude-fable-5", "claude-fable-5-2"
	h := newModelCatalogHarness(t, map[string][]claude.APIModel{
		key: {
			{ID: modelID, DisplayName: "Original Fable", ContextWindow: 1000000, MaxOutputTokens: 64000},
			{ID: retiredID, DisplayName: "Retired Fable"},
		},
	})
	construct := h.agent.newClaudeClient
	h.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		client := construct(log, options)
		transport := h.launches[len(h.launches)-1]
		replacement := len(h.launches) > 1
		levels := []any{effortLow, effortHigh}
		displayName := "Native Fable"
		if replacement {
			levels = []any{effortHigh}
			displayName = "Refreshed native Fable"
		}
		transport.initialize["models"] = []any{map[string]any{
			"value": modelID, "resolvedModel": modelID, "displayName": displayName,
			"supportedEffortLevels": levels, "supportsAutoMode": !replacement,
		}}
		transport.settings["applied"] = map[string]any{"model": modelID, "effort": effortLow}

		return client
	}

	cwd := t.TempDir()
	options := ClaudeOptions{Model: modelID, Env: map[string]string{providerAuthEnvAnthropicAPIKey: key}}
	created, err := h.agent.NewSession(t.Context(), NewSessionRequest(cwd, WithSessionMeta(options.Meta())))
	require.NoError(t, err)
	require.Contains(t, catalogModelValues(t, created.ConfigOptions), retiredID)
	require.NotContains(t, catalogModelValues(t, created.ConfigOptions), addedID)
	_, err = h.agent.SetSessionConfigOption(t.Context(), SetConfigOptionRequest(created.SessionId, configMode, acp.SessionConfigValueId(modeAuto)))
	require.NoError(t, err)
	connection, ok := h.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	beforeUpdates := len(connection.Updates())

	h.catalog.mu.Lock()
	h.catalog.models[key] = []claude.APIModel{
		{
			ID: modelID, DisplayName: "Refreshed Fable", ContextWindow: 2000000, MaxOutputTokens: 32000,
			SupportedEffortLevels: []string{effortLow, effortHigh, effortMax},
		},
		{ID: addedID, DisplayName: "New Fable"},
	}
	h.catalog.mu.Unlock()
	session := h.agent.sessions[created.SessionId]
	previous := session.currentClient()
	require.NoError(t, previous.Close())
	require.NoError(t, session.ensureClientAlive(t.Context()))
	require.NotSame(t, previous, session.currentClient())
	require.Len(t, h.launches, 2)
	require.Equal(t, key, h.launches[1].LaunchEnvironment()[providerAuthEnvAnthropicAPIKey])
	accesses, _, closes := h.catalog.snapshot()
	require.Equal(t, []claude.ModelCatalogAccess{
		{Endpoint: "https://api.anthropic.com/v1/models", Credential: key},
		{Endpoint: "https://api.anthropic.com/v1/models", Credential: key},
	}, accesses)
	require.Zero(t, closes)

	var updates [][]acp.SessionConfigOption
	for _, notification := range connection.Updates()[beforeUpdates:] {
		if update := notification.Update.ConfigOptionUpdate; update != nil {
			require.Equal(t, created.SessionId, notification.SessionId)
			updates = append(updates, update.ConfigOptions)
		}
	}
	require.Len(t, updates, 1, "a relaunch publishes its complete replacement configuration")
	require.Contains(t, catalogModelValues(t, updates[0]), addedID)
	require.NotContains(t, catalogModelValues(t, updates[0]), retiredID)
	byConfig := make(map[acp.SessionConfigId]*acp.SessionConfigOptionSelect)
	for _, option := range updates[0] {
		if option.Select != nil {
			byConfig[option.Select.Id] = option.Select
		}
	}
	require.Equal(t, acp.SessionConfigValueId(modeDefault), byConfig[configMode].CurrentValue)
	require.Equal(t, acp.SessionConfigValueId(effortHigh), byConfig[configEffort].CurrentValue)
	require.Equal(t, acp.SessionConfigValueId(modelID), byConfig[configModel].CurrentValue)
	choices := make(map[acp.SessionConfigValueId]acp.SessionConfigSelectOption)
	for _, option := range *byConfig[configModel].Options.Ungrouped {
		choices[option.Value] = option
	}
	require.Equal(t, "Refreshed native Fable", choices[modelID].Name)
	require.Equal(t, map[string]any{claudeMetaKey: map[string]any{
		claudeModelMetaContextWindowKey: int64(2000000), claudeModelMetaMaxOutputTokensKey: int64(32000),
		claudeModelMetaSupportedEffortKey: []string{effortHigh},
	}}, choices[modelID].Meta, "fresh API facts retain the replacement's narrower native capabilities")

	reused, err := h.agent.ResumeSession(t.Context(), ResumeSessionRequest(created.SessionId, cwd, WithSessionMeta(options.Meta())))
	require.NoError(t, err)
	require.Equal(t, updates[0], reused.ConfigOptions)
	meta, ok := reused.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, modelID, meta[claudeModelMetaModelIDKey])
	require.Equal(t, effortHigh, meta[claudeModelMetaVariantKey])
	require.Equal(t, []string{effortHigh}, meta[claudeModelMetaAvailableVariantsKey])

	var appliedEffort, appliedMode any
	for _, sent := range h.launches[1].Sent() {
		request, ok := sent.(claude.ControlRequest)
		if !ok {
			continue
		}
		switch request.Request["subtype"] {
		case "apply_flag_settings":
			settings, ok := request.Request["settings"].(map[string]any)
			require.True(t, ok)
			if effort, exists := settings["effort"]; exists {
				appliedEffort = effort
			}
		case "set_permission_mode":
			appliedMode = request.Request["mode"]
		}
	}
	require.Equal(t, effortHigh, appliedEffort)
	require.Equal(t, string(modeDefault), appliedMode)
}

func TestModelCatalogNativeDefaultSurvivesEmptyAllowlist(t *testing.T) {
	const key = "synthetic-default-api-key"
	const resolvedDefault = "claude-sonnet-5"
	for _, selection := range []string{"native default", "explicit model", "environment model"} {
		t.Run(selection, func(t *testing.T) {
			newAuthSeams(t)
			h := newModelCatalogHarness(t, map[string][]claude.APIModel{
				key: {{ID: resolvedDefault, DisplayName: "Sonnet 5", ContextWindow: 1000000}},
			})
			construct := h.agent.newClaudeClient
			h.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
				client := construct(log, options)
				transport := h.launches[len(h.launches)-1]
				transport.initialize["models"] = []any{map[string]any{
					"value": modelDefault, "resolvedModel": resolvedDefault, "displayName": "Default",
				}}
				transport.settings = map[string]any{
					"applied":   map[string]any{"model": resolvedDefault},
					"effective": map[string]any{settingsFieldAvailableModels: []any{}},
				}

				return client
			}
			options := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: key}}
			switch selection {
			case "explicit model":
				options.Model = resolvedDefault
			case "environment model":
				options.Env[envAnthropicModel] = resolvedDefault
			}
			created, err := h.agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMeta(options.Meta())))
			if selection != "native default" {
				requireExactUnsupportedField(t, err, "model")

				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{modelDefault}, catalogModelValues(t, created.ConfigOptions))
			require.Equal(t, acp.SessionConfigValueId(modelDefault), catalogModelOption(t, created.ConfigOptions).CurrentValue)
			for _, sent := range h.launches[0].Sent() {
				if request, ok := sent.(claude.ControlRequest); ok {
					require.NotEqual(t, "set_model", request.Request["subtype"], "the already applied native default needs no model change")
				}
			}
		})
	}
}

func TestModelCatalogNativeFamilyAllowlistPreservesOlderVersions(t *testing.T) {
	newAuthSeams(t)
	const key = "synthetic-family-api-key"
	const latestID = "claude-sonnet-5"
	const olderID = "claude-sonnet-4-6"
	h := newModelCatalogHarness(t, map[string][]claude.APIModel{
		key: {
			{ID: latestID, DisplayName: "Sonnet 5"},
			{ID: olderID, DisplayName: "Sonnet 4.6"},
		},
	})
	construct := h.agent.newClaudeClient
	h.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		client := construct(log, options)
		transport := h.launches[len(h.launches)-1]
		transport.initialize["models"] = []any{
			map[string]any{"value": modelDefault, "resolvedModel": latestID, "displayName": "Default"},
			map[string]any{"value": "sonnet", "resolvedModel": latestID, "displayName": "Sonnet"},
		}
		transport.settings = map[string]any{
			"applied":   map[string]any{"model": latestID},
			"effective": map[string]any{settingsFieldAvailableModels: []any{"sonnet"}},
		}

		return client
	}
	options := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: key}}
	created, err := h.agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMeta(options.Meta())))
	require.NoError(t, err)
	require.Contains(t, catalogModelValues(t, created.ConfigOptions), olderID)

	selected, err := h.agent.SetSessionConfigOption(t.Context(), SetModelRequest(created.SessionId, olderID))
	require.NoError(t, err)
	require.Equal(t, acp.SessionConfigValueId(olderID), catalogModelOption(t, selected.ConfigOptions).CurrentValue)
	var sentModel any
	for _, sent := range h.launches[0].Sent() {
		if request, ok := sent.(claude.ControlRequest); ok && request.Request["subtype"] == "set_model" {
			sentModel = request.Request["model"]
		}
	}
	require.Equal(t, olderID, sentModel)
}

func TestModelCatalogNativeAllowlistKeepsSelectionRestrictions(t *testing.T) {
	const key = "synthetic-restricted-api-key"
	const latestID = "claude-sonnet-5"
	const olderID = "claude-sonnet-4-6"
	const datedID = "claude-sonnet-4-6-20260101"
	const similarID = "claude-sonnet-4-60"
	const legacyID = "claude-3-5-sonnet-20241022"
	const disabledID = "claude-sonnet-4-5-20250929"
	const opusID = "claude-opus-4-6"
	const unrelatedID = "claude-sonnetish-1"
	const fableID = "claude-fable-5"
	const newerFableID = "claude-fable-5-1"
	ids := []string{latestID, olderID, datedID, similarID, legacyID, disabledID, opusID, unrelatedID, fableID, newerFableID}
	for _, test := range []struct {
		name      string
		allowlist []any
		allowed   []string
	}{
		{"family", []any{"sonnet"}, []string{latestID, olderID, datedID, similarID, legacyID}},
		{"normalized family", []any{" SONNET[1m] "}, []string{latestID, olderID, datedID, similarID, legacyID}},
		{"version prefix narrows family", []any{"sonnet", olderID}, []string{olderID, datedID}},
		{"dated ID narrows family", []any{"sonnet", datedID}, []string{datedID}},
		{"version prefix without claude", []any{"sonnet-4-6"}, []string{olderID, datedID}},
		{"other family remains independent", []any{"sonnet", opusID}, []string{latestID, olderID, datedID, similarID, legacyID, opusID}},
		{"minor version extends prefix", []any{fableID}, []string{fableID, newerFableID}},
		{"minor version excludes parent", []any{newerFableID}, []string{newerFableID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			newAuthSeams(t)
			rows := make([]claude.APIModel, len(ids))
			for i, id := range ids {
				rows[i] = claude.APIModel{ID: id, DisplayName: "Sonnet"}
			}
			h := newModelCatalogHarness(t, map[string][]claude.APIModel{key: rows})
			construct := h.agent.newClaudeClient
			h.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
				client := construct(log, options)
				transport := h.launches[len(h.launches)-1]
				transport.initialize["models"] = []any{
					map[string]any{"value": modelDefault, "resolvedModel": latestID, "displayName": "Default"},
					map[string]any{"value": "sonnet", "resolvedModel": latestID, "displayName": "Sonnet"},
					map[string]any{"value": disabledID, "displayName": "Restricted Sonnet", "disabled": true},
				}
				transport.settings = map[string]any{
					"applied":   map[string]any{"model": modelDefault},
					"effective": map[string]any{settingsFieldAvailableModels: test.allowlist},
				}

				return client
			}
			options := ClaudeOptions{Env: map[string]string{providerAuthEnvAnthropicAPIKey: key}}
			created, err := h.agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMeta(options.Meta())))
			require.NoError(t, err)
			advertised := catalogModelValues(t, created.ConfigOptions)
			for _, id := range ids {
				before := len(h.launches[0].Sent())
				selected, selectionErr := h.agent.SetSessionConfigOption(t.Context(), SetModelRequest(created.SessionId, id))
				if !slices.Contains(test.allowed, id) {
					require.NotContains(t, advertised, id)
					requireExactUnsupportedField(t, selectionErr, jsonFieldValue)
					require.Len(t, h.launches[0].Sent(), before, "a denied model must not reach native selection")

					continue
				}
				require.Contains(t, advertised, id)
				require.NoError(t, selectionErr)
				require.Equal(t, acp.SessionConfigValueId(id), catalogModelOption(t, selected.ConfigOptions).CurrentValue)
				var sentModel any
				for _, sent := range h.launches[0].Sent()[before:] {
					if request, ok := sent.(claude.ControlRequest); ok && request.Request["subtype"] == "set_model" {
						sentModel = request.Request["model"]
					}
				}
				require.Equal(t, id, sentModel)
			}
		})
	}
}

func TestModelCatalogNativeFamilyAllowlistPreservesExplicitResolution(t *testing.T) {
	const sourceID = "claude-sonnet-5"
	const opaqueID = "deployment-catalog-42"
	const fableID = "claude-fable-5-1"
	for _, test := range []struct {
		name      string
		value     string
		resolved  string
		allowlist []any
		allowed   bool
	}{
		{"native alias", "sonnet", opaqueID, []any{"sonnet"}, true},
		{"native alias exact target", opaqueID, opaqueID, []any{"sonnet"}, true},
		{"opaque version override", sourceID, opaqueID, []any{"sonnet"}, true},
		{"cross-family version override", sourceID, fableID, []any{"sonnet"}, true},
		{"target family cannot allow source", sourceID, fableID, []any{"fable"}, false},
		{"target ID cannot allow source", sourceID, fableID, []any{fableID}, false},
		{"narrowed alias", "sonnet", opaqueID, []any{"sonnet", "claude-sonnet-4-6"}, false},
		{"narrowed alias exact target", opaqueID, opaqueID, []any{"sonnet", "claude-sonnet-4-6"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			newAuthSeams(t)
			h := newModelCatalogHarness(t, nil, WithClaudeDirectAPI(false))
			construct := h.agent.newClaudeClient
			h.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
				client := construct(log, options)
				transport := h.launches[len(h.launches)-1]
				models := []any{
					map[string]any{"value": modelDefault, "displayName": "Default"},
					map[string]any{"value": "sonnet", "resolvedModel": test.resolved, "displayName": "Pinned Sonnet"},
				}
				if test.value != "sonnet" {
					models = append(models, map[string]any{"value": test.value, "resolvedModel": test.resolved, "displayName": "Native model"})
				}
				transport.initialize["models"] = models
				transport.settings = map[string]any{
					"applied": map[string]any{"model": modelDefault},
					"effective": map[string]any{
						settingsFieldAvailableModels: test.allowlist,
						"modelOverrides":             map[string]any{sourceID: test.resolved},
					},
				}

				return client
			}
			created, err := h.agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
			require.NoError(t, err)
			before := len(h.launches[0].Sent())
			selected, err := h.agent.SetSessionConfigOption(t.Context(), SetModelRequest(created.SessionId, test.value))
			if !test.allowed {
				require.NotContains(t, catalogModelValues(t, created.ConfigOptions), test.value)
				requireExactUnsupportedField(t, err, jsonFieldValue)
				require.Len(t, h.launches[0].Sent(), before)

				return
			}
			require.Contains(t, catalogModelValues(t, created.ConfigOptions), test.value)
			require.NoError(t, err)
			require.Equal(t, acp.SessionConfigValueId(test.value), catalogModelOption(t, selected.ConfigOptions).CurrentValue)
			var sentModel any
			for _, sent := range h.launches[0].Sent()[before:] {
				if request, ok := sent.(claude.ControlRequest); ok && request.Request["subtype"] == "set_model" {
					sentModel = request.Request["model"]
				}
			}
			require.Equal(t, test.value, sentModel)
		})
	}
}
