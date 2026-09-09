package claude

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelCatalogBareModeUsesOnlyNativeSupportedAuthentication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		bare     bool
		simple   string
		oauth    bool
		eligible bool
	}{
		{name: "ordinary OAuth", oauth: true, eligible: true},
		{name: "bare OAuth", bare: true, oauth: true},
		{name: "simple one OAuth", simple: "1", oauth: true},
		{name: "simple true OAuth", simple: " TRUE ", oauth: true},
		{name: "simple yes OAuth", simple: "yes", oauth: true},
		{name: "simple on OAuth", simple: "on", oauth: true},
		{name: "simple zero OAuth", simple: "0", oauth: true, eligible: true},
		{name: "simple false OAuth", simple: "false", oauth: true, eligible: true},
		{name: "bare API key", bare: true, eligible: true},
		{name: "simple API key", simple: "true", eligible: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := newCatalogControlTransport()
			transport.environment = map[string]string{"CLAUDE_CODE_SIMPLE": test.simple}
			credentialEnv := directAPIKeyEnv
			if test.oauth {
				credentialEnv = directAPIOAuthTokenEnv
			}
			transport.environment[credentialEnv] = "synthetic-credential"
			client := NewClient(nil, Options{
				Bare: test.bare,
				Env:  map[string]string{"CLAUDE_CODE_SIMPLE": "false"},
			}, transport)
			startClientForTest(t, client)
			calls := 0
			catalog := modelCatalogReadFunc(func(_ context.Context, access ModelCatalogAccess) ([]APIModel, error) {
				calls++
				require.Equal(t, ModelCatalogAccess{
					Endpoint: modelCatalogEndpoint, Credential: "synthetic-credential", OAuth: test.oauth,
				}, access)

				return []APIModel{{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1"}}, nil
			})
			models, _, err := client.DiscoverModels(t.Context(), catalog, true)
			require.NoError(t, err)
			if test.eligible {
				require.Equal(t, 1, calls)
				require.Len(t, models, 2)
				require.Equal(t, "claude-fable-5-1", models[1].Value)
			} else {
				require.Zero(t, calls, "native ignores OAuth in bare mode, so its token must not leave through discovery")
				require.Equal(t, client.InitializeInfo().Models, models)
			}
		})
	}
}
