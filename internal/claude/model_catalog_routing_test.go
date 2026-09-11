package claude

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

const anthropicHaikuID = anthropicModelPrefix + "haiku-4-5"

// nativeMenu is the shape Claude reports for its own models: family aliases
// resolving to Anthropic ids, beside whatever else the process can dispatch.
func nativeMenu() []any {
	return []any{
		map[string]any{"value": anthropicModelDefault, "resolvedModel": "claude-opus-5", "displayName": "Default"},
		map[string]any{"value": anthropicModelSonnet, "resolvedModel": "claude-sonnet-5", "displayName": "Sonnet"},
		map[string]any{"value": "opus[1m]", "displayName": "Opus 1M"},
		map[string]any{"value": anthropicHaikuID, "displayName": "Haiku"},
		map[string]any{"value": "vendor/qwen", "displayName": "Qwen"},
	}
}

func discoveredValues(t *testing.T, transport *catalogControlTransport) []string {
	t.Helper()

	client := NewClient(nil, Options{}, transport)
	startClientForTest(t, client)

	models, _, err := client.DiscoverModels(context.Background(), nil, false)
	require.NoError(t, err)

	return modelValues(models)
}

func modelValues(models []AvailableModelInfo) []string {
	values := make([]string, 0, len(models))
	for _, model := range models {
		values = append(values, model.Value)
	}

	return values
}

// TestNativeMenuStandsForEveryProviderClaudeNames pins the scope of the
// withdrawal. Every provider Claude names but `firstParty` serves Anthropic's
// models under its own account, so its menu is exactly what Claude reports,
// as is that of a first-party process not pointed anywhere else.
func TestNativeMenuStandsForEveryProviderClaudeNames(t *testing.T) {
	t.Parallel()

	full := []string{anthropicModelDefault, anthropicModelSonnet, "opus[1m]", anthropicHaikuID, "vendor/qwen"}

	for _, tc := range []struct {
		name        string
		apiProvider string
		environment map[string]string
	}{
		{name: "first party", apiProvider: apiProviderFirstParty},
		{
			name:        "first party at Anthropic's own endpoint",
			apiProvider: apiProviderFirstParty,
			environment: map[string]string{directAPIBaseURLEnv: directAPIDefaultBase},
		},
		{name: "bedrock", apiProvider: "bedrock", environment: map[string]string{directAPIBedrockEnv: "1"}},
		{name: "vertex", apiProvider: "vertex", environment: map[string]string{directAPIVertexEnv: "1"}},
		{name: "foundry", apiProvider: "foundry", environment: map[string]string{directAPIFoundryEnv: "1"}},
		{
			// Claude reads a falsey switch as off and stays first party, so the
			// switch alone is not a route.
			name:        "switch set to a falsey value",
			apiProvider: apiProviderFirstParty,
			environment: map[string]string{directAPIBedrockEnv: "0"},
		},
		{
			// A gateway a provider already serves: bedrock keeps its own menu
			// whatever base URL is left in the environment.
			name:        "bedrock beside a stale base URL",
			apiProvider: "bedrock",
			environment: map[string]string{directAPIBaseURLEnv: "https://gateway.example"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newCatalogControlTransport()
			transport.models = nativeMenu()
			transport.apiProvider = tc.apiProvider
			transport.environment = tc.environment

			require.Equal(t, full, discoveredValues(t, transport))
		})
	}
}

// TestGatewayRoutedProcessWithdrawsAnthropicNames pins the one case the rule
// exists for: the Anthropic API spoken to another endpoint, which serves none
// of Claude's own model names.
func TestGatewayRoutedProcessWithdrawsAnthropicNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		environment map[string]string
		settings    map[string]any
	}{
		{
			name:        "base URL in the launch environment",
			environment: map[string]string{directAPIBaseURLEnv: "http://127.0.0.1:4000"},
		},
		{
			// Claude applies settings `env` over the process environment, so a
			// base URL named there is the one it used.
			name:     "base URL from settings",
			settings: map[string]any{"env": map[string]any{directAPIBaseURLEnv: "http://127.0.0.1:4000"}},
		},
		{
			name:        "settings base URL over an Anthropic environment",
			environment: map[string]string{directAPIBaseURLEnv: directAPIDefaultBase},
			settings:    map[string]any{"env": map[string]any{directAPIBaseURLEnv: "http://127.0.0.1:4000"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newCatalogControlTransport()
			transport.models = nativeMenu()
			transport.apiProvider = apiProviderFirstParty
			transport.environment = tc.environment
			if tc.settings != nil {
				transport.settings = tc.settings
			}

			require.Equal(t, []string{"vendor/qwen"}, discoveredValues(t, transport))
		})
	}
}

// TestGatewayRoutedProcessReadsTheServedList pins where the gateway's own
// models arrive: Claude discovers them after startup and serves them from
// list_models, so the startup snapshot is not the list to publish.
func TestGatewayRoutedProcessReadsTheServedList(t *testing.T) {
	t.Parallel()

	transport := newCatalogControlTransport()
	transport.models = nativeMenu()
	transport.listed = append(nativeMenu(), map[string]any{
		"value": "claude-sonnet-4-5-gw", "displayName": "GW Sonnet",
	}, map[string]any{"value": "vendor/deepseek", "displayName": "DeepSeek"})
	transport.apiProvider = apiProviderFirstParty
	transport.environment = map[string]string{directAPIBaseURLEnv: "http://127.0.0.1:4000"}

	require.Equal(t, []string{"vendor/qwen", "vendor/deepseek"}, discoveredValues(t, transport))
}

// TestGatewayRoutedProcessKeepsNativeRefusals pins the invariant the catalog
// merge depends on: a model Claude refused stays in the list whatever the
// route, so an allowlist or an explicit selection cannot reintroduce it.
func TestGatewayRoutedProcessKeepsNativeRefusals(t *testing.T) {
	t.Parallel()

	transport := newCatalogControlTransport()
	refused := map[string]any{"value": anthropicModelSonnet, "resolvedModel": "claude-sonnet-5"}
	refused[nativeModelDisabledKey] = true
	transport.models = []any{refused, map[string]any{"value": "vendor/qwen"}}
	transport.apiProvider = apiProviderFirstParty
	transport.environment = map[string]string{directAPIBaseURLEnv: "http://127.0.0.1:4000"}

	client := NewClient(nil, Options{}, transport)
	startClientForTest(t, client)

	models, _, err := client.DiscoverModels(context.Background(), nil, false)
	require.NoError(t, err)
	require.True(t, ModelDisabled(anthropicModelSonnet, models))
	require.True(t, ModelDisabled("claude-sonnet-5", models))
}

// TestGatewayRoutedProcessFallsBackToTheStartupSnapshot pins that a list Claude
// will not serve does not republish the names the route withdraws.
func TestGatewayRoutedProcessFallsBackToTheStartupSnapshot(t *testing.T) {
	t.Parallel()

	transport := newCatalogControlTransport()
	transport.models = nativeMenu()
	transport.apiProvider = apiProviderFirstParty
	transport.environment = map[string]string{directAPIBaseURLEnv: "http://127.0.0.1:4000"}
	transport.nativeFailed = true

	require.Equal(t, []string{"vendor/qwen"}, discoveredValues(t, transport))
}

// TestUnreadableSettingsStillHonourTheRoute pins the failure path: a settings
// read the process will not answer must not republish the Anthropic names,
// because the launch environment already establishes the route.
func TestUnreadableSettingsStillHonourTheRoute(t *testing.T) {
	t.Parallel()

	transport := newCatalogControlTransport()
	transport.models = nativeMenu()
	transport.apiProvider = apiProviderFirstParty
	transport.environment = map[string]string{directAPIBaseURLEnv: "http://127.0.0.1:4000"}
	transport.settingsFailed = true

	client := NewClient(nil, Options{}, transport)
	startClientForTest(t, client)

	models, settings, err := client.DiscoverModels(context.Background(), nil, false)
	require.Error(t, err)
	require.Nil(t, settings)
	require.Equal(t, []string{"vendor/qwen"}, modelValues(models))
}

// TestUnestablishedRouteWithdrawsAnthropicNames pins the two ways the route
// question goes unanswered. A provider Claude will not name and a launch
// environment the adapter cannot read both leave the route unestablished, and
// an unestablished route is no evidence that Anthropic's own names dispatch.
func TestUnestablishedRouteWithdrawsAnthropicNames(t *testing.T) {
	t.Parallel()

	t.Run("provider Claude will not name", func(t *testing.T) {
		t.Parallel()

		transport := newCatalogControlTransport()
		transport.models = nativeMenu()
		transport.apiProvider = ""

		require.Equal(t, []string{"vendor/qwen"}, discoveredValues(t, transport))
	})

	t.Run("unreadable launch environment", func(t *testing.T) {
		t.Parallel()

		transport := newCatalogControlTransport()
		transport.models = nativeMenu()
		transport.apiProvider = apiProviderFirstParty
		transport.environment = map[string]string{directAPIBaseURLEnv: directAPIDefaultBase}

		client := NewClient(nil, Options{}, transport)
		startClientForTest(t, client)
		require.NoError(t, client.Close())

		models, _, err := client.DiscoverModels(context.Background(), nil, false)
		require.Error(t, err)
		require.Equal(t, []string{"vendor/qwen"}, modelValues(models))
	})
}

func TestAnthropicModelIdentity(t *testing.T) {
	t.Parallel()

	type identityCase struct {
		model AvailableModelInfo
		want  bool
	}

	cases := make([]identityCase, 0, 8+len(anthropicModelAliases))
	cases = append(cases,
		identityCase{model: AvailableModelInfo{Value: "Opus[1m]"}, want: true},
		identityCase{model: AvailableModelInfo{Value: " " + anthropicModelSonnet + " "}, want: true},
		identityCase{model: AvailableModelInfo{Value: "claude-sonnet-5"}, want: true},
		// A resolved target decides: an alias Claude points at another model
		// dispatches to that model, whatever the alias is called.
		identityCase{model: AvailableModelInfo{Value: anthropicModelSonnet, ResolvedModel: "claude-sonnet-5"}, want: true},
		identityCase{model: AvailableModelInfo{Value: anthropicModelSonnet, ResolvedModel: "vendor/qwen"}},
		identityCase{model: AvailableModelInfo{Value: "vendor/qwen"}},
		identityCase{model: AvailableModelInfo{Value: "gpt-5"}},
		identityCase{model: AvailableModelInfo{}},
	)

	// Every alias the withdrawal knows about is covered, so a new one cannot be
	// added without a case for it.
	for _, alias := range anthropicModelAliases {
		cases = append(cases, identityCase{model: AvailableModelInfo{Value: alias}, want: true})
	}

	for _, tc := range cases {
		t.Run(tc.model.Value+"/"+tc.model.ResolvedModel, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, AnthropicModelIdentity(tc.model))
		})
	}
}

// TestFirstPartyRouteMatchesTheHarnessTest pins the routing predicate to what
// Claude does, not to what is tidy. On every spelling here Claude keeps its own
// menu and skips gateway discovery, so withholding on any of them would blank a
// menu Claude serves.
func TestFirstPartyRouteMatchesTheHarnessTest(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"unset":              {},
		"canonical":          {directAPIBaseURLEnv: "https://api.anthropic.com"},
		"path":               {directAPIBaseURLEnv: "https://api.anthropic.com/v1"},
		"query":              {directAPIBaseURLEnv: "https://api.anthropic.com/?beta=1"},
		"plain http":         {directAPIBaseURLEnv: "http://api.anthropic.com"},
		"explicit port":      {directAPIBaseURLEnv: "https://api.anthropic.com:443/v1"},
		"staging":            {directAPIBaseURLEnv: "https://api-staging.anthropic.com"},
		"mixed case host":    {directAPIBaseURLEnv: "https://API.Anthropic.com"},
		"assume switch on":   {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: "1"},
		"assume switch yes":  {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: "yes"},
		"assume switch true": {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: switchValueTrue},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.True(t, firstPartyRoute(env), "Claude serves its own menu on this route")
		})
	}

	for name, env := range map[string]map[string]string{
		"gateway":             {directAPIBaseURLEnv: "http://127.0.0.1:4000"},
		"assume switch zero":  {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: "0"},
		"assume switch false": {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: "false"},
		"assume switch empty": {directAPIBaseURLEnv: "http://127.0.0.1:4000", assumeFirstPartyEnv: ""},
		"lookalike host":      {directAPIBaseURLEnv: "https://api.anthropic.com.evil.test"},
		"unparseable":         {directAPIBaseURLEnv: "://"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.False(t, firstPartyRoute(env), "Claude routes elsewhere and discovers for itself here")
		})
	}
}

// TestFirstPartySpellingsKeepTheNativeMenu wires the route test to the catalog.
// Each of these is a route Claude serves its own menu on, so the adapter
// publishes that menu rather than blanking it on the stricter direct-API
// spelling test.
func TestFirstPartySpellingsKeepTheNativeMenu(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"path on the anthropic host": {directAPIBaseURLEnv: "https://api.anthropic.com/v1"},
		"plain http":                 {directAPIBaseURLEnv: "http://api.anthropic.com"},
		"staging host":               {directAPIBaseURLEnv: "https://api-staging.anthropic.com"},
		"assumed first party": {
			directAPIBaseURLEnv: "http://127.0.0.1:4000",
			assumeFirstPartyEnv: "1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			transport := newCatalogControlTransport()
			transport.models = nativeMenu()
			transport.listed = nativeMenu()
			transport.apiProvider = apiProviderFirstParty
			transport.environment = env

			require.Equal(t,
				[]string{anthropicModelDefault, anthropicModelSonnet, "opus[1m]", anthropicHaikuID, "vendor/qwen"},
				discoveredValues(t, transport),
				"Claude skips gateway discovery here and serves its compiled menu")
		})
	}
}

func TestSettingsOverrideFirstPartyAssumption(t *testing.T) {
	for _, tc := range []struct {
		name, launch, setting string
		want                  []string
	}{
		{"settings enables", "0", "1", []string{"default", "sonnet", "opus[1m]", "claude-haiku-4-5", "vendor/qwen"}},
		{"settings disables", "1", "0", []string{"vendor/qwen"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newCatalogControlTransport()
			transport.models = nativeMenu()
			transport.apiProvider = apiProviderFirstParty
			transport.environment = map[string]string{directAPIBaseURLEnv: "https://gateway.example", assumeFirstPartyEnv: tc.launch}
			transport.settings = map[string]any{"env": map[string]any{assumeFirstPartyEnv: tc.setting}}
			require.Equal(t, tc.want, discoveredValues(t, transport))
		})
	}
}

func TestRouteChangesDuringCatalogRead(t *testing.T) {
	transport := newCatalogControlTransport()
	transport.models = nativeMenu()
	transport.apiProvider = apiProviderFirstParty
	transport.environment = map[string]string{directAPIKeyEnv: "synthetic-key"}
	transport.settings = map[string]any{}
	client := NewClient(nil, Options{}, transport)
	startClientForTest(t, client)
	catalog := modelCatalogReadFunc(func(context.Context, ModelCatalogAccess) ([]APIModel, error) {
		transport.settings = map[string]any{"env": map[string]any{directAPIBaseURLEnv: "https://gateway.example"}}

		return []APIModel{{ID: "claude-api-only"}}, nil
	})
	models, _, err := client.DiscoverModels(t.Context(), catalog, true)
	require.NoError(t, err)
	require.Equal(t, []string{"vendor/qwen"}, modelValues(models))
}

// TestCredentialReportGovernsTheNativeMenu pins the second withholding
// condition. Claude names its own credential, and that report binds: an
// explicit `none` with no API key beside it is the process saying nothing signs
// its requests, so the names it cannot dispatch leave the menu. Every other
// answer — a bearer variable, an API key, or the silence a subscription and a
// cloud provider both keep — leaves the menu Claude is serving.
func TestCredentialReportGovernsTheNativeMenu(t *testing.T) {
	t.Parallel()

	full := []string{anthropicModelDefault, anthropicModelSonnet, "opus[1m]", anthropicHaikuID, "vendor/qwen"}

	for _, tc := range []struct {
		name         string
		apiProvider  string
		tokenSource  string
		apiKeySource string
		want         []string
	}{
		{
			name:        "no credential at all",
			apiProvider: apiProviderFirstParty,
			tokenSource: nativeTokenSourceNone,
			want:        []string{"vendor/qwen"},
		},
		{
			name:         "an API key signs the process",
			apiProvider:  apiProviderFirstParty,
			tokenSource:  nativeTokenSourceNone,
			apiKeySource: "ANTHROPIC_API_KEY",
			want:         full,
		},
		{
			name:        "a bearer signs the process",
			apiProvider: apiProviderFirstParty,
			tokenSource: "ANTHROPIC_AUTH_TOKEN",
			want:        full,
		},
		{
			name:        "a subscription names no source",
			apiProvider: apiProviderFirstParty,
			want:        full,
		},
		{
			name:        "a cloud provider names no source",
			apiProvider: "bedrock",
			want:        full,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := newCatalogControlTransport()
			transport.models = nativeMenu()
			transport.apiProvider = tc.apiProvider
			transport.tokenSource = tc.tokenSource
			transport.apiKeySource = tc.apiKeySource

			require.Equal(t, tc.want, discoveredValues(t, transport))
		})
	}
}

// TestUncredentialedProcessKeepsRefusedRows pins what survives the credential
// withdrawal: the rows Claude explicitly refused, so no allowlist or explicit
// selection can reintroduce one, beside whatever the process can still name.
func TestUncredentialedProcessKeepsRefusedRows(t *testing.T) {
	t.Parallel()

	transport := newCatalogControlTransport()
	transport.apiProvider = apiProviderFirstParty
	transport.tokenSource = nativeTokenSourceNone
	transport.models = []any{
		map[string]any{"value": anthropicModelSonnet, "resolvedModel": "claude-sonnet-5"},
		map[string]any{"value": "vendor/qwen"},
	}
	transport.listed = []any{
		map[string]any{"value": anthropicModelSonnet, "resolvedModel": "claude-sonnet-5"},
		map[string]any{"value": anthropicHaikuID, nativeModelDisabledKey: true},
		map[string]any{"value": "vendor/qwen"},
	}

	require.Equal(t, []string{anthropicHaikuID, "vendor/qwen"}, discoveredValues(t, transport))
}
