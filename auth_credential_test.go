package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretMethodsSaveAndHarvestExactlyOnceWithoutNativeAuth(t *testing.T) {
	for _, testCase := range []struct {
		method      string
		environment string
		message     string
	}{
		{authMethodSetupToken, providerAuthEnvClaudeOAuthToken, authMethodSetupTokenMessage},
		{authMethodAPIKey, providerAuthEnvAnthropicAPIKey, authMethodAPIKeyMessage},
	} {
		t.Run(testCase.method, func(t *testing.T) {
			seams := newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			generation := authCatalogGeneration(t, broker, sessionID)

			params := authorizeParams(sessionID, generation)
			params[authFieldMethod] = testCase.method
			result, err := broker.authorize(t.Context(), authParams(t, params))
			require.NoError(t, err)

			flow, ok := result.(authAuthorizeResult)
			require.True(t, ok)
			require.Equal(t, authInteractionSecret, flow.Interaction)
			require.Equal(t, testCase.message, flow.Message)
			require.Equal(t, map[string]string{
				authMethodSetupToken: authMethodSetupTokenInput,
				authMethodAPIKey:     authMethodAPIKeyInput,
			}[testCase.method], flow.CallbackInput)
			require.Empty(t, flow.URL)
			require.Zero(t, seams.loginCalls)
			require.Zero(t, seams.statusCalls)

			_, err = broker.callback(t.Context(), authParams(t, map[string]any{
				authFieldSessionID:  string(sessionID),
				authFieldProviderID: authProviderID,
				authFieldMethod:     testCase.method,
				authFieldFlowID:     flow.FlowID,
				authFieldInput:      "secret-value",
			}))
			require.NoError(t, err)

			status, err := broker.status(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			require.NoError(t, err)
			reported, ok := status.(authStatusResult)
			require.True(t, ok)
			require.Equal(t, authStateSaved, reported.State)
			require.Zero(t, seams.loginCalls)
			require.Zero(t, seams.statusCalls)

			harvested, err := broker.credential(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			require.NoError(t, err)
			binding, ok := harvested.(authCredentialResult)
			require.True(t, ok)
			require.Equal(t, "secret-value", binding.Credential.API.Key)
			require.Equal(t, map[string]string{"env": testCase.environment}, binding.Credential.API.Metadata)

			broker.mu.Lock()
			require.Nil(t, broker.byID[flow.FlowID].credential)
			broker.mu.Unlock()

			_, err = broker.credential(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			requireAuthFailed(t, err, authCauseFlowState)
			_, err = broker.callback(t.Context(), authParams(t, map[string]any{
				authFieldSessionID: string(sessionID), authFieldProviderID: authProviderID,
				authFieldMethod: testCase.method, authFieldFlowID: flow.FlowID, authFieldInput: "again",
			}))
			requireAuthFailed(t, err, authCauseFlowState)
		})
	}
}

func TestCredentialRejectsNativeLoginWithoutTouchingNativeResidence(t *testing.T) {
	seams := newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	flow := startAuthFlow(t, broker, sessionID)
	before := seams.statusCalls

	_, err := broker.credential(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	requireAuthFailed(t, err, authCausePolicy)
	require.Equal(t, before, seams.statusCalls)
	require.Zero(t, seams.logoutCalls)
	require.Zero(t, seams.removeCalls)
}

func TestSavedCredentialIsScrubbedByCancelAndMaterialDisconnect(t *testing.T) {
	for _, operation := range []string{"cancel", "disconnect", "disconnect-write-fail"} {
		t.Run(operation, func(t *testing.T) {
			seams := newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			generation := authCatalogGeneration(t, broker, sessionID)
			params := authorizeParams(sessionID, generation)
			params[authFieldMethod] = authMethodSetupToken

			result, err := broker.authorize(t.Context(), authParams(t, params))
			require.NoError(t, err)
			flow, ok := result.(authAuthorizeResult)
			require.True(t, ok)

			_, err = broker.callback(t.Context(), authParams(t, map[string]any{
				authFieldSessionID:  string(sessionID),
				authFieldProviderID: authProviderID,
				authFieldMethod:     authMethodSetupToken,
				authFieldFlowID:     flow.FlowID,
				authFieldInput:      "secret-value",
			}))
			require.NoError(t, err)

			switch operation {
			case "cancel":
				_, err = broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
			case "disconnect":
				_, err = broker.disconnect(t.Context(), authParams(t, map[string]any{
					authFieldSessionID:         string(sessionID),
					authFieldProviderID:        authProviderID,
					authFieldConnectionID:      testConnectionID,
					authFieldBindingGeneration: int64(1),
				}))
			case "disconnect-write-fail":
				original := ledgerRename
				writes := 0
				t.Cleanup(func() { ledgerRename = original })
				ledgerRename = func(oldPath, newPath string) error {
					writes++
					if writes == 2 {
						return errors.New("rename")
					}

					return original(oldPath, newPath)
				}
				_, err = broker.disconnect(t.Context(), authParams(t, map[string]any{
					authFieldSessionID:         string(sessionID),
					authFieldProviderID:        authProviderID,
					authFieldConnectionID:      testConnectionID,
					authFieldBindingGeneration: int64(1),
				}))
			}
			if operation == "disconnect-write-fail" {
				requireAuthFailed(t, err, authCauseProcess)
			} else {
				require.NoError(t, err)
			}

			broker.mu.Lock()
			saved := broker.byID[flow.FlowID]
			require.Nil(t, saved.credential)
			require.Equal(t, authStateCancelled, saved.state)
			broker.mu.Unlock()
			require.Zero(t, seams.loginCalls)
			require.Zero(t, seams.logoutCalls)
			require.Zero(t, seams.removeCalls)
		})
	}
}

func TestProviderCredentialDecodeIsStrictAndMetadataIsClosed(t *testing.T) {
	valid := `{"type":"api","key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}}`
	var credential ProviderCredential
	require.NoError(t, json.Unmarshal([]byte(valid), &credential))
	encoded, err := json.Marshal(credential)
	require.NoError(t, err)
	require.JSONEq(t, valid, string(encoded))
	_, err = json.Marshal(ProviderCredential{})
	require.ErrorIs(t, err, errProviderCredentialInvalid)

	for _, invalid := range []string{
		`[]`,
		`{`,
		`{"type":`,
		`{"key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":1,"key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"oauth","key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","type":"api","key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","key":"secret","extra":true,"metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","key":"","metadata":{"env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","key":"secret","metadata":{}}`,
		`{"type":"api","key":"secret","metadata":{"other":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","key":"secret","metadata":{"env":"CLAUDE_CODE_OAUTH_TOKEN","env":"ANTHROPIC_API_KEY"}}`,
		`{"type":"api","key":"secret","metadata":{"env":"ANTHROPIC_AUTH_TOKEN"}}`,
		`{"type":"api","key":"secret","metadata":{"env":"ANTHROPIC_API_KEY","extra":"x"}}`,
		`{"type":"api","key":"secret","metadata":{"env":"ANTHROPIC_API_KEY"}} true`,
	} {
		require.Error(t, json.Unmarshal([]byte(invalid), &credential), invalid)
	}
	for _, invalid := range []string{`{"`, `{"x":`, `{"x":1`, `{"x":1} true`} {
		require.Error(t, credential.UnmarshalJSON([]byte(invalid)), invalid)
	}

	for _, invalid := range []ProviderCredential{
		{Type: ProviderCredentialAPI},
		{
			Type: ProviderCredentialAPI,
			API:  &ProviderAPICredential{},
		},
		{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key: "secret",
			},
		},
		{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key:      "secret",
				Metadata: map[string]string{"other": providerAuthEnvAnthropicAPIKey},
			},
		},
		{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key: "secret",
				Metadata: map[string]string{
					settingsFieldEnv: providerAuthEnvAnthropicAPIKey,
					"extra":          "value",
				},
			},
		},
		{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key:      "secret",
				Metadata: map[string]string{settingsFieldEnv: providerAuthEnvAnthropicToken},
			},
		},
	} {
		require.False(t, validProviderCredential(invalid))
	}
}

func TestCredentialHarvestFailureBranches(t *testing.T) {
	newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)
	params := authorizeParams(sessionID, generation)
	params[authFieldMethod] = authMethodAPIKey
	result, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)
	flow, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	release, admitted := broker.admitSlot(t.Context())
	require.True(t, admitted)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = broker.credential(ctx, authParams(t, flowParams(string(sessionID), flow.FlowID)))
	requireAuthFailed(t, err, authCauseTimeout)
	release()

	_, err = broker.credential(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))
	requireAuthFailed(t, err, authCauseHarvestFailed)
}

func TestSecretCallbackValidationAndAdmissionFailures(t *testing.T) {
	for _, input := range []string{"", "bad\nvalue", "bad\x00value", strings.Repeat("x", authMaxTextInputBytes+1)} {
		requireInvalidAuthField(t, validateAuthSecretValue(input), authFieldInput)
	}

	newAuthSeams(t)
	broker, sessionID := newAuthBroker(t)
	generation := authCatalogGeneration(t, broker, sessionID)
	params := authorizeParams(sessionID, generation)
	params[authFieldMethod] = authMethodAPIKey
	result, err := broker.authorize(t.Context(), authParams(t, params))
	require.NoError(t, err)
	flow, ok := result.(authAuthorizeResult)
	require.True(t, ok)
	_, err = broker.callback(t.Context(), authParams(t, map[string]any{
		authFieldSessionID: string(sessionID), authFieldProviderID: authProviderID,
		authFieldMethod: authMethodAPIKey, authFieldFlowID: flow.FlowID, authFieldInput: "",
	}))
	requireInvalidAuthField(t, err, authFieldInput)

	release, admitted := broker.admitSlot(t.Context())
	require.True(t, admitted)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = broker.callback(ctx, authParams(t, map[string]any{
		authFieldSessionID: string(sessionID), authFieldProviderID: authProviderID,
		authFieldMethod: authMethodAPIKey, authFieldFlowID: flow.FlowID, authFieldInput: "secret",
	}))
	requireAuthFailed(t, err, authCauseTimeout)
	release()
}

func TestSecretCallbackConfirmationFailureAndTerminalRace(t *testing.T) {
	for _, terminalRace := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal-%t", terminalRace), func(t *testing.T) {
			newAuthSeams(t)
			broker, sessionID := newAuthBroker(t)
			generation := authCatalogGeneration(t, broker, sessionID)
			params := authorizeParams(sessionID, generation)
			params[authFieldMethod] = authMethodAPIKey
			result, err := broker.authorize(t.Context(), authParams(t, params))
			require.NoError(t, err)
			flow, ok := result.(authAuthorizeResult)
			require.True(t, ok)

			original := ledgerRename
			t.Cleanup(func() { ledgerRename = original })
			ledgerRename = func(oldPath, newPath string) error {
				if !terminalRace {
					return errors.New("rename")
				}
				renameErr := original(oldPath, newPath)
				_, _ = broker.cancel(t.Context(), authParams(t, flowParams(string(sessionID), flow.FlowID)))

				return renameErr
			}

			_, err = broker.callback(t.Context(), authParams(t, map[string]any{
				authFieldSessionID: string(sessionID), authFieldProviderID: authProviderID,
				authFieldMethod: authMethodAPIKey, authFieldFlowID: flow.FlowID, authFieldInput: "secret",
			}))
			if terminalRace {
				requireAuthFailed(t, err, authCauseFlowState)
			} else {
				requireAuthFailed(t, err, authCauseProcess)
			}
		})
	}
}
