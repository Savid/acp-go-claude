package claudeacp

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	authMethodClaudeAI      = "claude-ai-login"
	authMethodConsole       = "console-login"
	authMethodGateway       = "gateway"
	authMetaTerminalAuth    = "terminal-auth"
	authMetaGateway         = "gateway"
	authMetaCommand         = "command"
	authMetaArgs            = "args"
	authMetaLabel           = "label"
	authGatewayProtocol     = "anthropic"
	envAnthropicAuthToken   = "ANTHROPIC_AUTH_TOKEN" //nolint:gosec // Environment variable name, not a credential value.
	envAnthropicBaseURL     = "ANTHROPIC_BASE_URL"
	envAnthropicHeaders     = "ANTHROPIC_CUSTOM_HEADERS"
	terminalAuthCLIMarker   = "--cli"
	terminalAuthClaudeFlag  = "-claude"
	terminalAuthHomeFlag    = "-claude-home"
	terminalAuthLabelClaude = "Claude Login"
	terminalAuthLabelAPI    = "Anthropic Console Login"
	claudeAuthCommand       = "auth"
	claudeAuthLogin         = "login"
	claudeAuthClaudeAI      = "--claudeai"
	claudeAuthConsole       = "--console"
	gatewayBaseURLKey       = "baseUrl"
)

type gatewayAuth struct {
	BaseURL string
	Headers map[string]string
}

func (a *Agent) authMethods(params acp.InitializeRequest) []acp.AuthMethod {
	clientCapabilities := params.ClientCapabilities
	supportsTerminal := clientCapabilities.Auth.Terminal || clientMetaBool(clientCapabilities.Meta, authMetaTerminalAuth)
	supportsGateway := clientMetaBool(clientCapabilities.Auth.Meta, authMetaGateway)

	methods := make([]acp.AuthMethod, 0, 3)
	if supportsTerminal {
		methods = append(methods, a.terminalAuthMethods(clientCapabilities)...)
	}

	if supportsGateway {
		methods = append(methods, gatewayAuthMethod())
	}

	return methods
}

func (a *Agent) terminalAuthMethods(clientCapabilities acp.ClientCapabilities) []acp.AuthMethod {
	supportsMetaTerminalAuth := clientMetaBool(clientCapabilities.Meta, authMetaTerminalAuth)

	methods := make([]acp.AuthMethod, 0, 2)
	if !a.options.HideClaudeAuth {
		methods = append(methods, terminalAuthMethod(a.options, terminalAuthSpec{
			ID:          authMethodClaudeAI,
			Name:        "Claude Subscription",
			Description: "Use Claude subscription",
			Label:       terminalAuthLabelClaude,
			ClaudeArgs:  []string{claudeAuthCommand, claudeAuthLogin, claudeAuthClaudeAI},
		}, supportsMetaTerminalAuth))
	}

	methods = append(methods, terminalAuthMethod(a.options, terminalAuthSpec{
		ID:          authMethodConsole,
		Name:        "Anthropic Console",
		Description: "Use Anthropic Console (API usage billing)",
		Label:       terminalAuthLabelAPI,
		ClaudeArgs:  []string{claudeAuthCommand, claudeAuthLogin, claudeAuthConsole},
	}, supportsMetaTerminalAuth))

	return methods
}

type terminalAuthSpec struct {
	ID          string
	Name        string
	Description string
	Label       string
	ClaudeArgs  []string
}

func terminalAuthMethod(options Options, spec terminalAuthSpec, includeMeta bool) acp.AuthMethod {
	description := spec.Description
	args := terminalAuthArgs(options, spec.ClaudeArgs...)
	method := acp.AuthMethodTerminalInline{
		Id:          spec.ID,
		Name:        spec.Name,
		Description: &description,
		Args:        args,
	}

	if includeMeta {
		method.Meta = map[string]any{
			authMetaTerminalAuth: map[string]any{
				authMetaCommand: os.Args[0],
				authMetaArgs:    args,
				authMetaLabel:   spec.Label,
			},
		}
	}

	return acp.AuthMethod{Terminal: &method}
}

func terminalAuthArgs(options Options, claudeArgs ...string) []string {
	args := []string{terminalAuthCLIMarker}
	if options.ClaudePath != "" {
		args = append(args, terminalAuthClaudeFlag, options.ClaudePath)
	}

	if options.ClaudeHome != "" {
		args = append(args, terminalAuthHomeFlag, options.ClaudeHome)
	}

	return append(args, claudeArgs...)
}

func gatewayAuthMethod() acp.AuthMethod {
	description := "Use a custom gateway to authenticate and access models"

	return acp.AuthMethod{
		Agent: &acp.AuthMethodAgent{
			Id:          authMethodGateway,
			Name:        "Custom model gateway",
			Description: &description,
			Meta: map[string]any{
				authMetaGateway: map[string]any{
					"protocol": authGatewayProtocol,
				},
			},
		},
	}
}

func (a *Agent) setGatewayAuth(auth *gatewayAuth) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.gatewayAuth = &gatewayAuth{
		BaseURL: auth.BaseURL,
		Headers: cloneStringMap(auth.Headers),
	}
}

func (a *Agent) clearGatewayAuthForLogout() []*Session {
	a.mu.Lock()
	if a.gatewayAuth == nil {
		a.mu.Unlock()

		return nil
	}

	a.gatewayAuth = nil

	sessions := make([]*Session, 0, len(a.sessions))
	for sessionID, session := range a.sessions {
		if !session.gatewayAuth {
			continue
		}

		sessions = append(sessions, session)
		delete(a.sessions, sessionID)
		a.deleteCachedPermissionRulesLocked(sessionID)
	}
	a.mu.Unlock()

	a.docsMu.Lock()
	for _, session := range sessions {
		delete(a.documents, session.id)
		delete(a.focusedDocuments, session.id)
	}
	a.docsMu.Unlock()

	return sessions
}

func (a *Agent) gatewayEnv() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.gatewayAuth == nil {
		return nil
	}

	return map[string]string{
		envAnthropicAuthToken: "",
		envAnthropicBaseURL:   a.gatewayAuth.BaseURL,
		envAnthropicHeaders:   formatGatewayHeaders(a.gatewayAuth.Headers),
	}
}

func parseGatewayAuthMeta(meta map[string]any) (*gatewayAuth, error) {
	rawGateway, ok := meta[authMetaGateway].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gateway auth metadata is required")
	}

	baseURL, ok := gatewayBaseURL(rawGateway)
	if !ok {
		return nil, fmt.Errorf("gateway baseUrl is required")
	}

	if err := validateGatewayBaseURL(baseURL); err != nil {
		return nil, err
	}

	headers := stringMapValue(rawGateway["headers"])
	if err := validateGatewayHeaders(headers); err != nil {
		return nil, err
	}

	return &gatewayAuth{
		BaseURL: baseURL,
		Headers: headers,
	}, nil
}

func gatewayBaseURL(rawGateway map[string]any) (string, bool) {
	baseURL, ok := rawGateway[gatewayBaseURLKey].(string)
	if !ok {
		return "", false
	}

	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", false
	}

	return baseURL, true
}

func validateGatewayBaseURL(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("gateway baseUrl must be an absolute HTTPS URL")
	}

	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("gateway baseUrl must use HTTPS")
	}

	return nil
}

func validateGatewayHeaders(headers map[string]string) error {
	for key, value := range headers {
		if !validGatewayHeaderName(key) {
			return fmt.Errorf("gateway header %q has an invalid name", key)
		}

		if !validGatewayHeaderValue(value) {
			return fmt.Errorf("gateway header %q has an invalid value", key)
		}
	}

	return nil
}

func validGatewayHeaderName(name string) bool {
	if name == "" {
		return false
	}

	for _, char := range name {
		if (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') {
			continue
		}

		switch char {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}

	return true
}

func validGatewayHeaderValue(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}

	return true
}

func formatGatewayHeaders(headers map[string]string) string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+headers[key])
	}

	return strings.Join(parts, "\n")
}

func stringMapValue(value any) map[string]string {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	result := make(map[string]string, len(raw))
	for key, value := range raw {
		if stringValue, ok := value.(string); ok {
			result[key] = stringValue
		}
	}

	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	return maps.Clone(values)
}

func clientMetaBool(meta map[string]any, key string) bool {
	value, _ := meta[key].(bool)

	return value
}
