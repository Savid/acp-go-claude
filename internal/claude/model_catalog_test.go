package claude

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

type modelCatalogReadFunc func(context.Context, ModelCatalogAccess) ([]APIModel, error)

func (f modelCatalogReadFunc) List(ctx context.Context, access ModelCatalogAccess) ([]APIModel, error) {
	return f(ctx, access)
}

type catalogControlTransport struct {
	*fakeTransport
	environment map[string]string
	models      []any
	// listed answers list_models when set, so a test can separate Claude's
	// startup snapshot from the list it serves once discovery has landed.
	listed         []any
	apiProvider    string
	tokenSource    string
	apiKeySource   string
	settings       map[string]any
	nativeFailed   bool
	settingsFailed bool
}

func (t *catalogControlTransport) LaunchEnvironment() map[string]string {
	return maps.Clone(t.environment)
}

func (t *catalogControlTransport) Send(ctx context.Context, payload any) error {
	if err := t.fakeTransport.Send(ctx, payload); err != nil {
		return err
	}

	request, ok := payload.(ControlRequest)
	if !ok {
		return nil
	}

	subtype, _ := request.Request["subtype"].(string)
	response := map[string]any{}
	switch subtype {
	case "initialize":
		response["models"] = t.models

		account := map[string]any{}
		for key, value := range map[string]string{
			"apiProvider":  t.apiProvider,
			"tokenSource":  t.tokenSource,
			"apiKeySource": t.apiKeySource,
		} {
			if value != "" {
				account[key] = value
			}
		}

		if len(account) > 0 {
			response["account"] = account
		}
	case "list_models":
		response["models"] = t.models
		if t.listed != nil {
			response["models"] = t.listed
		}
	case "get_settings":
		response["effective"] = t.settings
	}

	control := map[string]any{"request_id": request.RequestID, "subtype": "success", "response": response}
	if subtype == "list_models" && t.nativeFailed || subtype == "get_settings" && t.settingsFailed {
		control = map[string]any{"request_id": request.RequestID, "subtype": "error", "error": "private native failure"}
	}
	t.sendMessage(map[string]any{"type": "control_response", "response": control})

	return nil
}

func newCatalogControlTransport() *catalogControlTransport {
	return &catalogControlTransport{
		fakeTransport: newFakeTransport(),
		environment:   map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "synthetic-token"},
		// A signed-in first-party session, as Claude reports one: it names the
		// provider it resolved and names no credential source, because a
		// subscription is neither a bearer variable nor an API key.
		apiProvider: apiProviderFirstParty,
		settings:    map[string]any{},
		models: []any{
			map[string]any{"value": "sonnet", "resolvedModel": "claude-sonnet-5", "displayName": "Sonnet"},
		},
	}
}

func TestModelCatalogUsesCapturedCredentialAndRechecksSettings(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "credential changed"}[changed], func(t *testing.T) {
			t.Parallel()
			transport := newCatalogControlTransport()
			client := NewClient(nil, Options{
				Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "other-options-token"},
				Authority: &NativeAuthority{NativeEnvironment: func() map[string]string {
					panic("catalog must not query a changed host environment")
				}},
			}, transport)
			startClientForTest(t, client)
			calls := 0
			catalog := modelCatalogReadFunc(func(_ context.Context, access ModelCatalogAccess) ([]APIModel, error) {
				calls++
				require.Equal(t, ModelCatalogAccess{
					Endpoint: modelCatalogEndpoint, Credential: "synthetic-token", OAuth: true,
				}, access)
				if changed {
					transport.settings = map[string]any{"env": map[string]any{"CLAUDE_CODE_OAUTH_TOKEN": "changed-token"}}
				}

				return []APIModel{{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1", ContextWindow: 1000000}}, nil
			})
			models, settings, err := client.DiscoverModels(t.Context(), catalog, true)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, transport.settings, settings.Effective)
			if changed {
				require.Len(t, models, 1)
			} else {
				require.Len(t, models, 2)
				require.Equal(t, "claude-fable-5-1", models[1].Value)
				require.Equal(t, int64(1000000), models[1].ContextWindow)
			}
		})
	}
}

func TestModelCatalogIneligibleSessionsNeverConsultProviderCache(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		env      map[string]string
		settings map[string]any
		direct   bool
	}{
		{name: "logged out", env: map[string]string{}, settings: map[string]any{}, direct: true},
		{name: "disabled", env: map[string]string{"ANTHROPIC_API_KEY": "synthetic-key"}, settings: map[string]any{}},
		{name: "ambiguous auth", env: map[string]string{"ANTHROPIC_API_KEY": "key", "CLAUDE_CODE_OAUTH_TOKEN": "token"}, settings: map[string]any{}, direct: true},
		{name: "native helper", env: map[string]string{"ANTHROPIC_API_KEY": "key"}, settings: map[string]any{"apiKeyHelper": "helper"}, direct: true},
		{name: "gateway", env: map[string]string{"ANTHROPIC_API_KEY": "key"}, settings: map[string]any{"forceLoginMethod": "gateway"}, direct: true},
		{name: "settings unavailable", env: map[string]string{"ANTHROPIC_API_KEY": "key"}, direct: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := newCatalogControlTransport()
			transport.environment = test.env
			transport.settings = test.settings
			client := NewClient(nil, Options{}, transport)
			startClientForTest(t, client)
			catalog := modelCatalogReadFunc(func(context.Context, ModelCatalogAccess) ([]APIModel, error) {
				t.Fatal("ineligible session consulted the provider cache")

				return nil, nil
			})
			models, _, err := client.DiscoverModels(t.Context(), catalog, test.direct)
			require.NoError(t, err)
			require.Equal(t, client.InitializeInfo().Models, models)
			require.Len(t, transport.sentPayloads(), 2, "only initialize and get_settings are needed")
		})
	}
}

func TestModelCatalogAccessRejectsRoutingAndCredentialAmbiguity(t *testing.T) {
	t.Parallel()
	for _, base := range []string{
		"http://api.anthropic.com", "https://api.anthropic.com:444", "https://other.example",
		"https://api.anthropic.com/v1", "https://api.anthropic.com?", "https://api.anthropic.com?q=value",
		"https://api.anthropic.com#fragment", "https://user:secret@api.anthropic.com",
		"https://api.anthropic.com/%2f", "https://api.anthropic.com.evil.example", "http://127.0.0.1",
	} {
		_, ok := resolveModelCatalogAccess(map[string]string{"ANTHROPIC_API_KEY": "key", "ANTHROPIC_BASE_URL": base})
		require.False(t, ok, base)
	}
	for _, key := range []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN", "ANTHROPIC_UNIX_SOCKET", "CLAUDE_CODE_API_BASE_URL",
	} {
		_, ok := resolveModelCatalogAccess(map[string]string{"ANTHROPIC_API_KEY": "key", key: "configured"})
		require.False(t, ok, key)
	}
	for _, key := range []string{"", " key", "key\n", "key\tvalue", "nonascii-λ"} {
		_, ok := resolveModelCatalogAccess(map[string]string{"ANTHROPIC_API_KEY": key})
		require.False(t, ok)
	}
	for _, base := range []string{"", "https://api.anthropic.com", "https://api.anthropic.com/", "https://API.ANTHROPIC.COM:443"} {
		access, ok := resolveModelCatalogAccess(map[string]string{"ANTHROPIC_API_KEY": "key", "ANTHROPIC_BASE_URL": base})
		require.True(t, ok)
		require.Equal(t, ModelCatalogAccess{Endpoint: modelCatalogEndpoint, Credential: "key"}, access)
	}
}

func TestModelCatalogFailureKeepsNativeFacts(t *testing.T) {
	t.Parallel()
	for _, nativeFailed := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider error", true: "native restrictions unavailable"}[nativeFailed], func(t *testing.T) {
			t.Parallel()
			transport := newCatalogControlTransport()
			transport.nativeFailed = nativeFailed
			client := NewClient(nil, Options{}, transport)
			startClientForTest(t, client)
			calls := 0
			catalog := modelCatalogReadFunc(func(context.Context, ModelCatalogAccess) ([]APIModel, error) {
				calls++

				return nil, errors.New("catalog unavailable")
			})
			models, _, err := client.DiscoverModels(t.Context(), catalog, true)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private")
			require.Equal(t, client.InitializeInfo().Models, models)
			if nativeFailed {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestModelCatalogReconciliationPreservesExactNativeRestrictions(t *testing.T) {
	t.Parallel()
	native := []AvailableModelInfo{
		{Value: "default", ResolvedModel: "claude-sonnet-5", DisplayName: "Default"},
		{Value: "fable", ResolvedModel: "claude-fable-5", DisplayName: "Fable", SupportedEffortLevels: []string{"high"}, SupportsAutoMode: true},
		{Value: "denied", ResolvedModel: "claude-denied-5", Disabled: true},
		{Value: "no-effort", ResolvedModel: "claude-no-effort", EffortUnsupported: true, SupportedEffortLevels: []string{"high"}},
	}
	api := []APIModel{
		{ID: "claude-fable-5", DisplayName: "Claude Fable 5", ContextWindow: 1000000, MaxOutputTokens: 64000, SupportedEffortLevels: []string{"low", "high", "max"}},
		{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1", ContextWindow: 2000000, SupportedEffortLevels: []string{"low", "max"}},
		{ID: "claude-denied-5", DisplayName: "Denied"},
		{ID: "claude-no-effort", DisplayName: "No effort", SupportedEffortLevels: []string{"high"}},
	}
	models := mergeModelCatalog(api, native, nil)
	byID := map[string]AvailableModelInfo{}
	for _, model := range models {
		byID[model.Value] = model
	}
	require.Equal(t, []string{"high"}, byID["claude-fable-5"].SupportedEffortLevels)
	require.True(t, byID["claude-fable-5"].SupportsAutoMode)
	require.Equal(t, []string{"low", "max"}, byID["claude-fable-5-1"].SupportedEffortLevels)
	require.False(t, byID["claude-fable-5-1"].SupportsAutoMode)
	require.Equal(t, int64(2000000), byID["claude-fable-5-1"].ContextWindow)
	require.Zero(t, byID["claude-fable-5-1"].MaxOutputTokens)
	require.True(t, ModelDisabled("claude-denied-5", models))
	require.True(t, byID["claude-denied-5"].Disabled)
	require.Empty(t, byID["claude-no-effort"].SupportedEffortLevels)
	require.Empty(t, byID["no-effort"].SupportedEffortLevels)
	require.False(t, ModelDisabled("unlisted", models))
	byID["claude-fable-5"].SupportedEffortLevels[0] = "changed"
	byID["claude-fable-5-1"].SupportedEffortLevels[0] = "changed"
	require.Equal(t, []string{"high"}, native[1].SupportedEffortLevels)
	require.Equal(t, []string{"low", "max"}, api[1].SupportedEffortLevels)
	cloned := CloneAvailableModels(models)
	cloned[1].SupportedEffortLevels[0] = "clone-change"
	require.Equal(t, []string{"high"}, models[1].SupportedEffortLevels)
}

func TestModelCatalogPreservesNativeCustomOptionAndDefaultResolution(t *testing.T) {
	t.Parallel()
	const modelID = "claude-fable-5-1"
	native := parseAvailableModels([]any{
		map[string]any{"value": "default", "resolvedModel": modelID, "displayName": "Default (Fable 5.1)"},
		map[string]any{"value": "sonnet", "resolvedModel": modelID, "displayName": "Configured Sonnet"},
		map[string]any{
			"value": modelID, "resolvedModel": modelID, "displayName": "Fable 5.1",
			"description": "My custom model", "supportsEffort": false,
			"supportedEffortLevels": []any{"high"},
		},
	})
	require.Empty(t, native[2].SupportedEffortLevels)
	models := mergeModelCatalog([]APIModel{{
		ID: modelID, DisplayName: "Claude Fable 5.1", ContextWindow: 1000000,
		MaxOutputTokens: 64000, SupportedEffortLevels: []string{"high"},
	}}, native, nil)
	require.Len(t, models, 3, "an exact custom option must not be duplicated")
	require.Equal(t, "Fable 5.1", models[2].DisplayName)
	require.Equal(t, "My custom model", models[2].Description)
	require.Empty(t, models[2].SupportedEffortLevels)
	for _, model := range models {
		require.Equal(t, int64(1000000), model.ContextWindow)
		require.Equal(t, int64(64000), model.MaxOutputTokens)
		require.False(t, model.SupportsAutoMode)
	}
}

func TestModelCatalogNativeOverridesRequireResolvedIdentityForAPIFacts(t *testing.T) {
	t.Parallel()
	const sourceID = "claude-source-5"
	const targetID = "claude-target-5"
	for _, nativeIdentity := range []string{"API only", "unresolved native", "resolved native"} {
		t.Run(nativeIdentity, func(t *testing.T) {
			t.Parallel()
			transport := newCatalogControlTransport()
			transport.settings["modelOverrides"] = map[string]any{sourceID: targetID}
			if nativeIdentity != "API only" {
				row := map[string]any{"value": sourceID, "displayName": "Native configured model"}
				if nativeIdentity == "resolved native" {
					row["resolvedModel"] = targetID
				}
				transport.models = append(transport.models, row)
			}
			client := NewClient(nil, Options{}, transport)
			startClientForTest(t, client)
			catalog := modelCatalogReadFunc(func(context.Context, ModelCatalogAccess) ([]APIModel, error) {
				return []APIModel{
					{ID: sourceID, DisplayName: "Source model", ContextWindow: 1000000, SupportedEffortLevels: []string{"high"}},
					{ID: targetID, DisplayName: "Target model", ContextWindow: 200000, SupportedEffortLevels: []string{"low"}},
				}, nil
			})
			models, _, err := client.DiscoverModels(t.Context(), catalog, true)
			require.NoError(t, err)
			var source *AvailableModelInfo
			for i := range models {
				if models[i].Value == sourceID {
					source = &models[i]
				}
			}
			require.NotNil(t, source, "native-owned mapping must preserve the requested dispatch value")
			if nativeIdentity == "resolved native" {
				require.Equal(t, targetID, source.ResolvedModel)
				require.Equal(t, int64(200000), source.ContextWindow)
				require.Equal(t, []string{"low"}, source.SupportedEffortLevels)
			} else {
				require.Empty(t, source.ResolvedModel)
				require.Zero(t, source.ContextWindow)
				require.Empty(t, source.SupportedEffortLevels)
			}
			if nativeIdentity == "API only" {
				require.Equal(t, sourceID, source.DisplayName)
			} else {
				require.Equal(t, "Native configured model", source.DisplayName)
			}
		})
	}
}
