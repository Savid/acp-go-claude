package claudeacp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthMethodsEnumeratesThePinnedCatalog(t *testing.T) {
	broker, sessionID := newAuthBroker(t)

	result, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	require.NoError(t, err)

	methods, ok := result.(authMethodsResult)
	require.True(t, ok)
	require.NotEmpty(t, methods.Generation)
	require.Len(t, methods.Providers, 1)
	require.Equal(t, []authMethodEntry{
		{ID: authMethodLogin, Type: authMethodTypeOAuth, Label: authMethodLoginLabel},
		{ID: authMethodSetupToken, Type: authMethodTypeAPI, Label: authMethodSetupTokenLabel},
		{ID: authMethodAPIKey, Type: authMethodTypeAPI, Label: authMethodAPIKeyLabel},
	}, methods.Providers[authProviderID])

	// The published entry is closed: no source, completeness, or prompt field
	// rides along.
	encoded, err := json.Marshal(methods.Providers[authProviderID][0])
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"login","type":"oauth","label":"Claude subscription"}`, string(encoded))

	second := authCatalogGeneration(t, broker, sessionID)
	require.NotEqual(t, methods.Generation, second)
}

func TestAuthMethodsRejectsAddressingFailures(t *testing.T) {
	broker, _ := newAuthBroker(t)

	_, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": "missing"}))
	requireInvalidAuthField(t, err, "sessionId")

	_, err = broker.methods(t.Context(), authParams(t, map[string]any{}))
	requireInvalidAuthField(t, err, "sessionId")

	_, err = broker.methods(t.Context(), json.RawMessage(`{"other":1}`))
	requireInvalidAuthField(t, err, "other")
}

func TestAuthMethodsFailsClosedWhenNoTokenCanBeMinted(t *testing.T) {
	broker, sessionID := newAuthBroker(t)

	original := authRandRead

	authRandRead = func([]byte) (int, error) { return 0, errTestRandom }

	t.Cleanup(func() { authRandRead = original })

	_, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	requireAuthFailed(t, err, authCauseProcess)
}

func TestPublishAuthCatalogDropsAndFailsClosed(t *testing.T) {
	entries, err := publishAuthCatalog(map[string][]authCatalogMethod{
		authProviderID: {
			{ID: "kept", Type: authMethodTypeOAuth, Label: "Kept"},
			{ID: "dropped", Type: authMethodTypeOAuth, Label: "bad\u202Elabel"},
		},
	})
	require.NoError(t, err)
	require.Len(t, entries[authProviderID], 1)
	require.Equal(t, "kept", entries[authProviderID][0].ID)

	_, err = publishAuthCatalog(map[string][]authCatalogMethod{
		authProviderID: {{ID: "only", Type: authMethodTypeOAuth, Label: "\u0000"}},
	})
	requireAuthFailed(t, err, authCauseNativeVeto)
}

func TestSecretPresentationRejectsAnUnsafeCatalogMessage(t *testing.T) {
	broker, _ := newAuthBroker(t)
	_, cause := broker.mintPresentation(t.Context(), &authFlow{
		method:    authCatalogMethod{Type: authMethodTypeAPI, Message: "\x00"},
		expiresAt: authNow().Add(time.Minute),
	})
	require.Equal(t, authCauseNativeVeto, cause)
}

func TestAuthDisplayTextNormalisesFirstAndBoundsTheNormalisedForm(t *testing.T) {
	// The decomposed form is three bytes; NFC composes it to two, and the
	// composed form is what is measured, relayed, and returned.
	normalized, ok := authDisplayText("e\u0301", 2)
	require.True(t, ok)
	require.Equal(t, "\u00e9", normalized)

	_, ok = authDisplayText("", 10)
	require.False(t, ok)

	_, ok = authDisplayText(strings.Repeat("a", 11), 10)
	require.False(t, ok)

	_, ok = authDisplayText("bad\u202Eoverride", authMaxLabelBytes)
	require.False(t, ok)

	_, ok = authDisplayText("tab\there", authMaxLabelBytes)
	require.False(t, ok)

	_, ok = authDisplayText(string([]byte{0xff, 0xfe}), authMaxLabelBytes)
	require.False(t, ok)

	spaced, ok := authDisplayText("Claude account 1 (+)", authMaxLabelBytes)
	require.True(t, ok)
	require.Equal(t, "Claude account 1 (+)", spaced)
}

func TestAuthDisplayURLBounds(t *testing.T) {
	bounded, ok := authDisplayURL("https://claude.com/oauth/authorize?a=b")
	require.True(t, ok)
	require.Equal(t, "https://claude.com/oauth/authorize?a=b", bounded)

	for _, candidate := range []string{
		"",
		"https://" + strings.Repeat("a", authMaxURLBytes),
		"http://claude.com/",
		"https://user:pass@claude.com/",
		"https://claude.com/#fragment",
		"https:///path",
		"https://claude.com/%zz",
	} {
		_, ok := authDisplayURL(candidate)
		require.False(t, ok, candidate)
	}
}

func TestValidateAuthInputsRejectsAnswersToPromptsNobodyAsked(t *testing.T) {
	require.NoError(t, validateAuthInputs(nil))
	require.NoError(t, validateAuthInputs(map[string]string{}))
	requireInvalidAuthField(t, validateAuthInputs(map[string]string{"instanceUrl": "x"}), authFieldInputs)
}

func TestAuthMethodsFailsClosedWhenNoCatalogCanBeProduced(t *testing.T) {
	broker, sessionID := newAuthBroker(t)

	original := pinnedAuthCatalog

	pinnedAuthCatalog = func() map[string][]authCatalogMethod {
		return map[string][]authCatalogMethod{authProviderID: {{ID: authMethodLogin, Label: ""}}}
	}

	t.Cleanup(func() { pinnedAuthCatalog = original })

	_, err := broker.methods(t.Context(), authParams(t, map[string]any{"sessionId": string(sessionID)}))
	requireAuthFailed(t, err, authCauseNativeVeto)
}
