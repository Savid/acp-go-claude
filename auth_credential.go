package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

type ProviderCredentialType string

const ProviderCredentialAPI ProviderCredentialType = "api"

const credentialFieldType = "type"

type ProviderAPICredential struct {
	Key      string            `json:"key"`
	Metadata map[string]string `json:"metadata"`
}

type ProviderCredential struct {
	Type ProviderCredentialType
	API  *ProviderAPICredential
}

type ProviderAuthBinding struct {
	ConnectionID      string             `json:"connectionId"`
	Revision          int64              `json:"revision"`
	BindingGeneration int64              `json:"bindingGeneration"`
	Credential        ProviderCredential `json:"credential"`
}

var errProviderCredentialInvalid = errors.New("provider credential is not a valid member of the closed union")

type apiCredentialEnvelope struct {
	Type ProviderCredentialType `json:"type"`
	ProviderAPICredential
}

func (credential ProviderCredential) MarshalJSON() ([]byte, error) {
	if !validProviderCredential(credential) {
		return nil, errProviderCredentialInvalid
	}

	return json.Marshal(apiCredentialEnvelope{Type: credential.Type, ProviderAPICredential: *credential.API})
}

func (credential *ProviderCredential) UnmarshalJSON(data []byte) error {
	fields, err := strictCredentialFields(data)
	if err != nil {
		return err
	}

	if len(fields) != 3 {
		return errProviderCredentialInvalid
	}

	raw, ok := fields[credentialFieldType]
	if !ok {
		return errProviderCredentialInvalid
	}

	var kind ProviderCredentialType
	if json.Unmarshal(raw, &kind) != nil || kind != ProviderCredentialAPI {
		return errProviderCredentialInvalid
	}

	var key string
	if json.Unmarshal(fields["key"], &key) != nil {
		return errProviderCredentialInvalid
	}

	metadataFields, err := strictCredentialFields(fields["metadata"])
	if err != nil || len(metadataFields) != 1 {
		return errProviderCredentialInvalid
	}

	var environment string
	if json.Unmarshal(metadataFields[settingsFieldEnv], &environment) != nil {
		return errProviderCredentialInvalid
	}

	value := ProviderCredential{
		Type: kind,
		API: &ProviderAPICredential{
			Key:      key,
			Metadata: map[string]string{settingsFieldEnv: environment},
		},
	}
	if !validProviderCredential(value) {
		return errProviderCredentialInvalid
	}

	*credential = value

	return nil
}

func validProviderCredential(credential ProviderCredential) bool {
	if credential.Type != ProviderCredentialAPI || credential.API == nil || credential.API.Key == "" {
		return false
	}

	metadata := credential.API.Metadata
	if len(metadata) != 1 {
		return false
	}

	environment, ok := metadata[settingsFieldEnv]
	if !ok {
		return false
	}

	return environment == providerAuthEnvClaudeOAuthToken || environment == providerAuthEnvAnthropicAPIKey
}

func strictCredentialFields(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errProviderCredentialInvalid
	}

	fields := map[string]json.RawMessage{}

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, errProviderCredentialInvalid
		}

		key, _ := keyToken.(string)
		if _, duplicate := fields[key]; duplicate {
			return nil, errProviderCredentialInvalid
		}

		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errProviderCredentialInvalid
		}

		fields[key] = value
	}

	if _, err := decoder.Token(); err != nil {
		return nil, errProviderCredentialInvalid
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errProviderCredentialInvalid
	}

	return fields, nil
}

type authCredentialResult struct {
	ConnectionID      string             `json:"connectionId"`
	Revision          int64              `json:"revision"`
	BindingGeneration int64              `json:"bindingGeneration"`
	Credential        ProviderCredential `json:"credential"`
}

func (p *providerAuth) credential(ctx context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	if flow.method.Type != authMethodTypeAPI {
		return nil, authFailed(authCausePolicy, flow.providerID, flow.method.ID, flow.id)
	}

	release, admitted := p.admitSlot(ctx)
	if !admitted {
		return nil, authFailed(authCauseTimeout, flow.providerID, flow.method.ID, flow.id)
	}
	defer release()

	record, ok, err := p.ledger.read(flow.providerID)
	if err != nil || !ok ||
		record.Method != flow.method.ID ||
		record.ConnectionID != flow.connectionID ||
		record.Revision != flow.revision ||
		record.BindingGeneration != flow.bindingGeneration ||
		record.State != authLedgerConfirmed {
		return nil, authFailed(authCauseHarvestFailed, flow.providerID, flow.method.ID, flow.id)
	}

	p.mu.Lock()
	if flow.state != authStateSaved || flow.harvested || len(flow.credential) == 0 {
		p.mu.Unlock()

		return nil, authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	flow.harvested = true
	key := string(flow.credential)
	flow.dropCredential()
	p.mu.Unlock()

	return authCredentialResult{
		ConnectionID:      flow.connectionID,
		Revision:          flow.revision,
		BindingGeneration: flow.bindingGeneration,
		Credential: ProviderCredential{
			Type: ProviderCredentialAPI,
			API: &ProviderAPICredential{
				Key:      key,
				Metadata: map[string]string{settingsFieldEnv: flow.method.Environment},
			},
		},
	}, nil
}
