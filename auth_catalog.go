package claudeacp

import (
	"context"
	"encoding/json"
	"net/url"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The pinned catalog exposes Claude's three first-party authentication paths.
const (
	authProviderID = "anthropic"

	authMethodLogin      = "login"
	authMethodSetupToken = "setup-token"
	authMethodAPIKey     = "api-key"
)

const (
	authMethodTypeOAuth = "oauth"
	authMethodTypeAPI   = "api"
)

const (
	authMethodLoginLabel      = "Claude subscription"
	authMethodSetupTokenLabel = "Claude setup token"
	authMethodAPIKeyLabel     = "Anthropic API key" //nolint:gosec // UI label, not a credential.

	authMethodLoginMessage      = "Sign in with your Claude subscription."
	authMethodSetupTokenMessage = "Run `claude setup-token`, then paste the generated token." //nolint:gosec // UI guidance, not a credential.
	authMethodAPIKeyMessage     = "Paste an API key from the Anthropic Console."              //nolint:gosec // UI guidance, not a credential.
	authMethodLoginInput        = "code"
	authMethodSetupTokenInput   = "setup token"
	authMethodAPIKeyInput       = "API key"
)

// Display-field bounds. A value violating its bound is dropped, never
// truncated, and the leg then fails closed.
const (
	authMaxURLBytes     = 2048
	authMaxMessageBytes = 2048
	authMaxLabelBytes   = 256
)

// authCatalogMethod is one entry of the current catalog.
type authCatalogMethod struct {
	ID            string
	Type          string
	Label         string
	Message       string
	Interaction   string
	CallbackInput string
	Environment   string
}

// authMethodEntry is one published catalog entry. Credentials are submitted
// through callback, so none of the methods carries pre-authorize prompts.
type authMethodEntry struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

type authMethodsResult struct {
	Providers  map[string][]authMethodEntry `json:"providers"`
	Generation string                       `json:"generation"`
}

// pinnedAuthCatalog is the whole catalog. It publishes no completeness claim
// and no source label, and free entry of an unlisted provider is never offered.
var pinnedAuthCatalog = func() map[string][]authCatalogMethod {
	return map[string][]authCatalogMethod{
		authProviderID: {
			{
				ID:            authMethodLogin,
				Type:          authMethodTypeOAuth,
				Label:         authMethodLoginLabel,
				Message:       authMethodLoginMessage,
				Interaction:   authInteractionCallback,
				CallbackInput: authMethodLoginInput,
			},
			{
				ID:            authMethodSetupToken,
				Type:          authMethodTypeAPI,
				Label:         authMethodSetupTokenLabel,
				Message:       authMethodSetupTokenMessage,
				Interaction:   authInteractionSecret,
				CallbackInput: authMethodSetupTokenInput,
				Environment:   providerAuthEnvClaudeOAuthToken,
			},
			{
				ID:            authMethodAPIKey,
				Type:          authMethodTypeAPI,
				Label:         authMethodAPIKeyLabel,
				Message:       authMethodAPIKeyMessage,
				Interaction:   authInteractionSecret,
				CallbackInput: authMethodAPIKeyInput,
				Environment:   providerAuthEnvAnthropicAPIKey,
			},
		},
	}
}

// methods enumerates the catalog and mints the generation naming this exact
// result. A method id means nothing outside the generation that produced it, so
// authorize must present the token back.
func (p *providerAuth) methods(_ context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	if _, sessionErr := p.authSession(sessionID); sessionErr != nil {
		return nil, sessionErr
	}

	catalog := pinnedAuthCatalog()

	entries, err := publishAuthCatalog(catalog)
	if err != nil {
		return nil, err
	}

	generation, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, "", "", "")
	}

	p.mu.Lock()
	p.generation = generation
	p.catalog = catalog
	p.mu.Unlock()

	return authMethodsResult{Providers: entries, Generation: generation}, nil
}

// publishAuthCatalog applies the label bound entry by entry. An entry whose
// label violates its bound is omitted and the leg still succeeds; the leg fails
// closed only when no catalog can be produced at all.
func publishAuthCatalog(catalog map[string][]authCatalogMethod) (map[string][]authMethodEntry, error) {
	entries := make(map[string][]authMethodEntry, len(catalog))

	for providerID, methods := range catalog {
		published := make([]authMethodEntry, 0, len(methods))

		for _, method := range methods {
			label, ok := authDisplayText(method.Label, authMaxLabelBytes)
			if !ok {
				continue
			}

			published = append(published, authMethodEntry{ID: method.ID, Type: method.Type, Label: label})
		}

		if len(published) == 0 {
			continue
		}

		entries[providerID] = published
	}

	if len(entries) == 0 {
		return nil, authFailed(authCauseNativeVeto, "", "", "")
	}

	return entries, nil
}

// authDisplayText normalises a presentation string to NFC and measures its
// bounds and categories on that normalised form, which is also the form the
// adapter relays, persists, and returns. Normalising after measuring bounds a
// string nobody sends.
func authDisplayText(value string, maxBytes int) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > maxBytes || !utf8.ValidString(normalized) {
		return "", false
	}

	for _, char := range normalized {
		if !authDisplayRune(char) {
			return "", false
		}
	}

	return normalized, true
}

// authDisplayRune restricts free text to Unicode categories L, N, P, S, and Zs.
// Every C* category is rejected, which is also what excludes every
// bidirectional override and embedding character: a label is the provider name
// in the one place a human decides which account to bind.
func authDisplayRune(char rune) bool {
	switch {
	case unicode.IsLetter(char), unicode.IsNumber(char), unicode.IsPunct(char), unicode.IsSymbol(char):
		return true
	case unicode.Is(unicode.Zs, char):
		return true
	default:
		return false
	}
}

// authDisplayURL applies the url bound: at most 2048 bytes, scheme exactly
// https, no userinfo, no fragment. It runs after the grammar's own independent
// URL validation, on the value that validation returned.
func authDisplayURL(value string) (string, bool) {
	normalized := norm.NFC.String(value)
	if normalized == "" || len(normalized) > authMaxURLBytes {
		return "", false
	}

	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != uriSchemeHTTPS || parsed.User != nil || parsed.Fragment != "" || parsed.Host == "" {
		return "", false
	}

	return normalized, true
}

// validateAuthInputs rejects answers to prompts the pinned catalog does not
// advertise.
func validateAuthInputs(inputs map[string]string) error {
	if len(inputs) > 0 {
		return invalidAuthField(authFieldInputs)
	}

	return nil
}
