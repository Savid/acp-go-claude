package claudeacp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testProviderAuthBinding(environment string) ProviderAuthBinding {
	return ProviderAuthBinding{
		ConnectionID:      testConnectionID,
		Revision:          2,
		BindingGeneration: 3,
		Credential: ProviderCredential{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key:      "secret-value",
				Metadata: map[string]string{"env": environment},
			},
		},
	}
}

func TestProviderAuthInjectionAppliesExactEnvironmentAndRecordsValuesFreeResidence(t *testing.T) {
	newAuthSeams(t)
	broker, _ := newAuthBroker(t)
	env := map[string]string{}
	binding := testProviderAuthBinding(providerAuthEnvClaudeOAuthToken)

	result := broker.inject(t.Context(), env, map[string]ProviderAuthBinding{authProviderID: binding})
	require.Equal(t, authInjectionApplied, result.outcome)
	require.Equal(t, "secret-value", env[providerAuthEnvClaudeOAuthToken])
	require.NotContains(t, env, providerAuthEnvAnthropicAPIKey)

	record, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authMethodSetupToken, record.Method)
	require.Equal(t, binding.ConnectionID, record.ConnectionID)
	require.Equal(t, binding.Revision, record.Revision)
	require.Equal(t, binding.BindingGeneration, record.BindingGeneration)

	encoded, err := ledgerMarshal(record)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-value")

	session := broker.agent.sessions[testSessionID]
	session.providerAuthResident = result.resident
	inventory, err := broker.inventory(t.Context(), authParams(t, map[string]any{
		authFieldSessionID: string(testSessionID),
	}))
	require.NoError(t, err)
	reported, ok := inventory.(authInventoryResult)
	require.True(t, ok)
	require.Equal(t, authProofConfirmedPresent, reported.Entries[0].ProofSource)
}

func TestProviderAuthInjectionConflictsWithAnyConfiguredAuthOwner(t *testing.T) {
	newAuthSeams(t)
	broker, _ := newAuthBroker(t)
	binding := testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)

	for _, name := range providerAuthCredentialEnvNames {
		env := map[string]string{name: "already-owned"}
		result := broker.inject(t.Context(), env, map[string]ProviderAuthBinding{authProviderID: binding})
		require.Equal(t, authInjectionConflict, result.outcome, name)
		require.NotEqual(t, "secret-value", env[providerAuthEnvAnthropicAPIKey], name)
	}
}

func TestProviderAuthInjectionClosedFailureBranches(t *testing.T) {
	newAuthSeams(t)
	broker, _ := newAuthBroker(t)
	require.Equal(t, authInjectionResult{}, broker.inject(t.Context(), map[string]string{}, nil))
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{"other": {}}).outcome)
	require.Equal(t, "", providerAuthFingerprint(nil))
	require.Equal(t, authErrValueInvalid, providerAuthFingerprint(map[string]ProviderAuthBinding{authProviderID: {}}))
	require.NotEmpty(t, providerAuthFingerprint(map[string]ProviderAuthBinding{
		authProviderID: testProviderAuthBinding(providerAuthEnvAnthropicAPIKey),
	}))

	release, admitted := broker.admitSlot(t.Context())
	require.True(t, admitted)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, authInjectionConflict, broker.inject(ctx, map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: testProviderAuthBinding(providerAuthEnvAnthropicAPIKey),
	}).outcome)
	release()

	originalRead := ledgerReadFile
	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: testProviderAuthBinding(providerAuthEnvAnthropicAPIKey),
	}).outcome)
	ledgerReadFile = originalRead

	first := testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)
	require.Equal(t, authInjectionApplied, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: first,
	}).outcome)
	conflict := first
	conflict.ConnectionID = "other"
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: conflict,
	}).outcome)
	conflict = first
	conflict.Revision++
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: conflict,
	}).outcome)
	conflict = testProviderAuthBinding(providerAuthEnvClaudeOAuthToken)
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: conflict,
	}).outcome)

	originalRename := ledgerRename
	ledgerRename = func(string, string) error { return errors.New("rename") }
	env := map[string]string{}
	next := first
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), env, map[string]ProviderAuthBinding{
		authProviderID: next,
	}).outcome)
	require.NotContains(t, env, providerAuthEnvAnthropicAPIKey)
	ledgerRename = originalRename
}

func TestProviderAuthInjectionCannotResurrectDisconnectedMaterial(t *testing.T) {
	newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	binding := testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)
	require.Equal(t, authInjectionApplied, broker.inject(t.Context(), map[string]string{}, map[string]ProviderAuthBinding{
		authProviderID: binding,
	}).outcome)

	_, err := broker.disconnect(t.Context(), authParams(t, map[string]any{
		authFieldSessionID:         string(sessionID),
		authFieldProviderID:        authProviderID,
		authFieldConnectionID:      binding.ConnectionID,
		authFieldBindingGeneration: binding.BindingGeneration,
	}))
	require.NoError(t, err)

	env := map[string]string{}
	require.Equal(t, authInjectionConflict, broker.inject(t.Context(), env, map[string]ProviderAuthBinding{
		authProviderID: binding,
	}).outcome)
	require.Empty(t, env)

	record, ok, err := broker.ledger.read(authProviderID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerRemoved, record.State)
	require.Equal(t, binding.BindingGeneration+1, record.BindingGeneration)
}

func TestProviderAuthInjectionOptionIsStrictAndSurfaceGated(t *testing.T) {
	binding := testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)
	meta := ClaudeOptions{ProviderAuth: map[string]ProviderAuthBinding{authProviderID: binding}}.Meta()

	_, err := claudeOptionsFromMeta(meta)
	requireInvalidAuthField(t, err, providerAuthOptionPath)

	options, err := claudeOptionsFromMetaWithProviderAuth(meta, true)
	require.NoError(t, err)
	require.Equal(t, binding, options.ProviderAuth[authProviderID])

	invalid := ClaudeOptions{ProviderAuth: map[string]ProviderAuthBinding{
		authProviderID: testProviderAuthBinding(providerAuthEnvAnthropicToken),
	}}.Meta()
	_, err = claudeOptionsFromMetaWithProviderAuth(invalid, true)
	requireInvalidAuthField(t, err, providerAuthOptionPath)

	wire := map[string]any{
		authProviderID: map[string]any{
			"connectionId":      testConnectionID,
			"revision":          float64(2),
			"bindingGeneration": float64(3),
			"credential": map[string]any{
				"type": "api", "key": "secret-value",
				"metadata": map[string]any{"env": providerAuthEnvAnthropicAPIKey},
			},
		},
	}
	parsed, err := providerAuthBindingsOption(wire)
	require.NoError(t, err)
	require.Equal(t, testConnectionID, parsed[authProviderID].ConnectionID)

	for _, invalid := range []any{
		"bad",
		map[string]any{},
		map[string]any{"other": map[string]any{}},
		map[string]any{authProviderID: func() {}},
		map[string]any{authProviderID: map[string]any{"extra": true}},
	} {
		_, err = providerAuthBindingsOption(invalid)
		require.Error(t, err)
	}
	invalidBinding := testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)
	invalidBinding.ConnectionID = ""
	_, err = providerAuthBindingsOption(map[string]ProviderAuthBinding{authProviderID: invalidBinding})
	requireInvalidAuthField(t, err, providerAuthOptionPath+"."+authProviderID)
	for _, connectionID := range []string{"bad connection", "é", strings.Repeat("x", authConnectionIDMaxBytes+1)} {
		invalidBinding = testProviderAuthBinding(providerAuthEnvAnthropicAPIKey)
		invalidBinding.ConnectionID = connectionID
		_, err = providerAuthBindingsOption(map[string]ProviderAuthBinding{authProviderID: invalidBinding})
		requireInvalidAuthField(t, err, providerAuthOptionPath+"."+authProviderID)
	}
	require.Nil(t, cloneProviderAuthBindings(nil))
}

func TestProviderAuthInjectionResponseReportsNoopForLiveSessionReuse(t *testing.T) {
	session := &agentSession{providerAuthInjection: authInjectionApplied}
	bindings := map[string]ProviderAuthBinding{
		authProviderID: testProviderAuthBinding(providerAuthEnvAnthropicAPIKey),
	}

	meta := sessionReuseResponseMeta(session, bindings)
	claudeMeta, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	providerAuth, ok := claudeMeta[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, authInjectionNoop, providerAuth["injection"])

	meta = sessionReuseResponseMeta(session, nil)
	claudeMeta, ok = meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	providerAuth, ok = claudeMeta[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, authInjectionApplied, providerAuth["injection"])

	conflict := &agentSession{providerAuthInjection: authInjectionConflict}
	meta = sessionReuseResponseMeta(conflict, bindings)
	claudeMeta, ok = meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	providerAuth, ok = claudeMeta[providerAuthCapabilityKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, authInjectionConflict, providerAuth["injection"])

	require.NotNil(t, sessionLoadResponseMeta(session, bindings, true))
	require.NotNil(t, sessionLoadResponseMeta(session, bindings, false))
}
