package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	authInjectionApplied  = "applied"
	authInjectionNoop     = "noop"
	authInjectionConflict = "conflict"
)

type authInjectedLineage struct {
	connectionID      string
	revision          int64
	bindingGeneration int64
	method            string
}

type authInjectionResult struct {
	outcome  string
	resident map[string]authInjectedLineage
}

func providerAuthFingerprint(bindings map[string]ProviderAuthBinding) string {
	if len(bindings) == 0 {
		return ""
	}

	encoded, err := json.Marshal(bindings)
	if err != nil {
		return authErrValueInvalid
	}

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}

func (p *providerAuth) inject(ctx context.Context, env map[string]string, bindings map[string]ProviderAuthBinding) authInjectionResult {
	if len(bindings) == 0 {
		return authInjectionResult{}
	}

	binding, ok := bindings[authProviderID]
	if !ok || !validProviderCredential(binding.Credential) {
		return authInjectionResult{outcome: authInjectionConflict}
	}

	environment := binding.Credential.API.Metadata[settingsFieldEnv]

	method := authMethodSetupToken
	if environment == providerAuthEnvAnthropicAPIKey {
		method = authMethodAPIKey
	}

	for _, name := range providerAuthCredentialEnvNames {
		if env[name] != "" {
			return authInjectionResult{outcome: authInjectionConflict}
		}
	}

	release, admitted := p.admitSlot(ctx)
	if !admitted {
		return authInjectionResult{outcome: authInjectionConflict}
	}
	defer release()

	record, exists, err := p.ledger.read(authProviderID)
	if err != nil {
		return authInjectionResult{outcome: authInjectionConflict}
	}

	if exists {
		if record.State == authLedgerRemoved {
			return authInjectionResult{outcome: authInjectionConflict}
		}

		if record.ConnectionID != binding.ConnectionID ||
			record.Revision != binding.Revision ||
			record.BindingGeneration != binding.BindingGeneration ||
			record.Method != method {
			return authInjectionResult{outcome: authInjectionConflict}
		}
	}

	env[environment] = binding.Credential.API.Key
	now := authNow().UnixMilli()

	createdAt := now
	if exists {
		createdAt = record.CreatedAt
	}

	record = authLedgerRecord{
		ProviderID:        authProviderID,
		Method:            method,
		ConnectionID:      binding.ConnectionID,
		Revision:          binding.Revision,
		BindingGeneration: binding.BindingGeneration,
		State:             authLedgerConfirmed,
		CreatedAt:         createdAt,
		UpdatedAt:         now,
	}
	if err := p.ledger.write(record); err != nil {
		delete(env, environment)

		return authInjectionResult{outcome: authInjectionConflict}
	}

	return authInjectionResult{
		outcome: authInjectionApplied,
		resident: map[string]authInjectedLineage{
			authProviderID: {
				connectionID:      binding.ConnectionID,
				revision:          binding.Revision,
				bindingGeneration: binding.BindingGeneration,
				method:            method,
			},
		},
	}
}
