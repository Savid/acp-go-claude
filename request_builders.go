package claudeacp

import "github.com/coder/acp-go-sdk"

// SessionRequestOption configures embedded-Go ACP session lifecycle requests.
type SessionRequestOption func(*sessionRequestConfig)

type sessionRequestConfig struct {
	additionalDirectories []string
	mcpServers            []acp.McpServer
	meta                  map[string]any
}

// NewSessionRequest constructs a session/new request with ACP-required empty
// slices initialized for embedded Go callers.
func NewSessionRequest(cwd string, opts ...SessionRequestOption) acp.NewSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.NewSessionRequest{
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// LoadSessionRequest constructs a session/load request with ACP-required empty
// slices initialized for embedded Go callers.
func LoadSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.LoadSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.LoadSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ResumeSessionRequest constructs a session/resume request.
func ResumeSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.ResumeSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.ResumeSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            config.stableMCPServers(),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// ForkSessionRequest constructs an unstable session/fork request.
func ForkSessionRequest(sessionID acp.SessionId, cwd string, opts ...SessionRequestOption) acp.UnstableForkSessionRequest {
	config := newSessionRequestConfig(opts...)

	return acp.UnstableForkSessionRequest{
		SessionId:             sessionID,
		Cwd:                   cwd,
		McpServers:            unstableMCPServersFromStable(config.stableMCPServers()),
		AdditionalDirectories: config.additionalDirectoriesClone(),
		Meta:                  cloneAnyMap(config.meta),
	}
}

// WithSessionMCPServers sets MCP servers for a session lifecycle request.
func WithSessionMCPServers(servers ...acp.McpServer) SessionRequestOption {
	cloned := cloneMCPServers(servers)

	return func(config *sessionRequestConfig) {
		config.mcpServers = cloneMCPServers(cloned)
	}
}

// WithSessionAdditionalDirectories sets additional workspace directories for a
// session lifecycle request.
func WithSessionAdditionalDirectories(paths ...string) SessionRequestOption {
	cloned := append([]string(nil), paths...)

	return func(config *sessionRequestConfig) {
		config.additionalDirectories = append([]string(nil), cloned...)
	}
}

// WithSessionMeta merges metadata into a session lifecycle request.
func WithSessionMeta(meta map[string]any) SessionRequestOption {
	cloned := cloneAnyMap(meta)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned)
	}
}

// WithSessionClaudeOptions merges Claude-specific options into a session
// lifecycle request's _meta.claude.options object.
func WithSessionClaudeOptions(options ClaudeOptions) SessionRequestOption {
	cloned := cloneClaudeOptions(options)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, cloned.Meta())
	}
}

// WithSessionRawSDKMessages toggles raw Claude SDK message emission for a
// session lifecycle request.
func WithSessionRawSDKMessages(enabled bool) SessionRequestOption {
	return func(config *sessionRequestConfig) {
		if config.meta == nil {
			config.meta = map[string]any{}
		}

		claudeMeta := ensureMetaMap(config.meta, claudeMetaKey)
		claudeMeta[emitRawSDKMessagesKey] = enabled
		config.meta[claudeMetaKey] = claudeMeta
	}
}

// WithSessionOutputFormat sets Claude structured output for a session lifecycle
// request.
func WithSessionOutputFormat(format ClaudeOutputFormat) SessionRequestOption {
	cloned := cloneOutputFormat(format)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, ClaudeOptions{OutputFormat: &cloned}.Meta())
	}
}

// WithSessionGoal sets initial _meta.claude.goal metadata for a session
// lifecycle request. It serializes only client-settable goal fields and does
// not send a /goal command to Claude.
func WithSessionGoal(goal ClaudeGoal) SessionRequestOption {
	value := clientGoalMap(goal)

	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, map[string]any{
			claudeMetaKey: map[string]any{
				claudeGoalMetaKey: value,
			},
		})
	}
}

// WithSessionGoalClear clears _meta.claude.goal metadata for a session
// lifecycle request.
func WithSessionGoalClear() SessionRequestOption {
	return func(config *sessionRequestConfig) {
		config.meta = mergeAnyMap(config.meta, map[string]any{
			claudeMetaKey: map[string]any{
				claudeGoalMetaKey: nil,
			},
		})
	}
}

// SetGoalRequest constructs params for the _claude/session/setGoal extension
// method. It serializes only client-settable goal fields and does not send a
// /goal command to Claude.
func SetGoalRequest(sessionID acp.SessionId, goal ClaudeGoal) map[string]any {
	return map[string]any{
		acpFieldSessionID: sessionID,
		claudeGoalMetaKey: clientGoalMap(goal),
	}
}

// ClearGoalRequest constructs params for the _claude/session/setGoal extension
// method clear operation.
func ClearGoalRequest(sessionID acp.SessionId) map[string]any {
	return map[string]any{
		acpFieldSessionID: sessionID,
		claudeGoalMetaKey: nil,
	}
}

func clientGoalMap(goal ClaudeGoal) map[string]any {
	value := map[string]any{
		goalFieldObjective: goal.Objective,
	}
	if goal.CompletionCondition != "" {
		value[goalFieldCompletionCondition] = goal.CompletionCondition
	}

	if goal.Status != "" {
		value[goalFieldStatus] = goal.Status
	}

	if goal.Reason != "" {
		value[goalFieldReason] = goal.Reason
	}

	return value
}

func newSessionRequestConfig(opts ...SessionRequestOption) sessionRequestConfig {
	config := sessionRequestConfig{}
	for _, opt := range opts {
		opt(&config)
	}

	return config
}

func (config sessionRequestConfig) stableMCPServers() []acp.McpServer {
	if config.mcpServers == nil {
		return []acp.McpServer{}
	}

	return cloneMCPServers(config.mcpServers)
}

func (config sessionRequestConfig) additionalDirectoriesClone() []string {
	return append([]string(nil), config.additionalDirectories...)
}

// PromptRequest constructs a session/prompt request with a non-nil prompt
// slice for embedded Go callers.
func PromptRequest(sessionID acp.SessionId, blocks ...acp.ContentBlock) acp.PromptRequest {
	return acp.PromptRequest{
		SessionId: sessionID,
		Prompt:    append([]acp.ContentBlock{}, blocks...),
	}
}

// TextPromptRequest constructs a session/prompt request containing one text
// content block.
func TextPromptRequest(sessionID acp.SessionId, text string) acp.PromptRequest {
	return PromptRequest(sessionID, acp.TextBlock(text))
}

// ListSessionsRequestOption configures embedded-Go session/list requests.
type ListSessionsRequestOption func(*acp.ListSessionsRequest)

// ListSessionsRequest constructs a session/list request.
func ListSessionsRequest(opts ...ListSessionsRequestOption) acp.ListSessionsRequest {
	var req acp.ListSessionsRequest
	for _, opt := range opts {
		opt(&req)
	}

	return req
}

// WithListSessionsCwd filters session/list by cwd.
func WithListSessionsCwd(cwd string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cwd
		req.Cwd = &value
	}
}

// WithListSessionsCursor sets the cursor for session/list pagination.
func WithListSessionsCursor(cursor string) ListSessionsRequestOption {
	return func(req *acp.ListSessionsRequest) {
		value := cursor
		req.Cursor = &value
	}
}

// WithListSessionsAdditionalDirectories filters session/list by additional
// workspace directories.
func WithListSessionsAdditionalDirectories(paths ...string) ListSessionsRequestOption {
	cloned := append([]string(nil), paths...)

	return func(req *acp.ListSessionsRequest) {
		req.AdditionalDirectories = append([]string(nil), cloned...)
	}
}

// WithListSessionsMeta sets metadata on a session/list request.
func WithListSessionsMeta(meta map[string]any) ListSessionsRequestOption {
	cloned := cloneAnyMap(meta)

	return func(req *acp.ListSessionsRequest) {
		req.Meta = mergeAnyMap(req.Meta, cloned)
	}
}

// ClaudeOption configures ClaudeOptions values.
type ClaudeOption func(*ClaudeOptions)

// NewClaudeOptions constructs ClaudeOptions from functional options.
func NewClaudeOptions(opts ...ClaudeOption) ClaudeOptions {
	options := ClaudeOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return cloneClaudeOptions(options)
}

// WithClaudeBare configures Claude bare mode.
func WithClaudeBare(enabled bool) ClaudeOption {
	return func(options *ClaudeOptions) {
		options.Bare = enabled
	}
}

// WithClaudeEnv configures Claude session environment overrides.
func WithClaudeEnv(env map[string]string) ClaudeOption {
	cloned := cloneStringMap(env)

	return func(options *ClaudeOptions) {
		options.Env = cloneStringMap(cloned)
	}
}

// WithClaudeSystemPrompt configures the Claude session system prompt.
func WithClaudeSystemPrompt(prompt string) ClaudeOption {
	return func(options *ClaudeOptions) {
		options.SystemPrompt = prompt
	}
}

// WithClaudeModel configures the initial Claude model.
func WithClaudeModel(model string) ClaudeOption {
	return func(options *ClaudeOptions) {
		options.Model = model
	}
}

// WithClaudePermissionMode configures the initial Claude permission mode.
func WithClaudePermissionMode(mode string) ClaudeOption {
	return func(options *ClaudeOptions) {
		options.PermissionMode = mode
	}
}

// WithClaudeAdditionalDirectories configures additional Claude workspace
// directories.
func WithClaudeAdditionalDirectories(paths ...string) ClaudeOption {
	cloned := append([]string(nil), paths...)

	return func(options *ClaudeOptions) {
		options.AdditionalDirectories = append([]string(nil), cloned...)
	}
}

// WithClaudeOutputFormat configures Claude structured output.
func WithClaudeOutputFormat(format ClaudeOutputFormat) ClaudeOption {
	cloned := cloneOutputFormat(format)

	return func(options *ClaudeOptions) {
		options.OutputFormat = &cloned
	}
}

// WithClaudeJSONSchema configures Claude JSON Schema structured output.
func WithClaudeJSONSchema(schema map[string]any) ClaudeOption {
	return WithClaudeOutputFormat(JSONSchemaOutputFormat(schema))
}

// JSONSchemaOutputFormat constructs a Claude JSON Schema structured output
// format.
func JSONSchemaOutputFormat(schema map[string]any) ClaudeOutputFormat {
	return ClaudeOutputFormat{
		Type:   ClaudeOutputFormatJSONSchema,
		Schema: cloneAnyMap(schema),
	}
}

func cloneClaudeOptions(options ClaudeOptions) ClaudeOptions {
	cloned := options
	cloned.Env = cloneStringMap(options.Env)

	cloned.AdditionalDirectories = append([]string(nil), options.AdditionalDirectories...)
	if options.OutputFormat != nil {
		outputFormat := cloneOutputFormat(*options.OutputFormat)
		cloned.OutputFormat = &outputFormat
	}

	return cloned
}

func cloneOutputFormat(format ClaudeOutputFormat) ClaudeOutputFormat {
	return ClaudeOutputFormat{
		Type:   format.Type,
		Schema: cloneAnyMap(format.Schema),
	}
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	result := cloneAnyMap(base)
	if result == nil {
		result = map[string]any{}
	}

	for key, value := range overlay {
		if valueMap, ok := value.(map[string]any); ok {
			if existingMap, ok := result[key].(map[string]any); ok {
				result[key] = mergeAnyMap(existingMap, valueMap)

				continue
			}
		}

		result[key] = cloneAny(value)
	}

	return result
}

func ensureMetaMap(meta map[string]any, key string) map[string]any {
	current, _ := meta[key].(map[string]any)
	if current == nil {
		current = map[string]any{}
	} else {
		current = cloneAnyMap(current)
	}

	meta[key] = current

	return current
}

func cloneMCPServers(servers []acp.McpServer) []acp.McpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.McpServer, len(servers))
	for index, server := range servers {
		cloned[index] = cloneMCPServer(server)
	}

	return cloned
}

func cloneMCPServer(server acp.McpServer) acp.McpServer {
	switch {
	case server.Http != nil:
		value := *server.Http
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Http: &value}
	case server.Sse != nil:
		value := *server.Sse
		value.Meta = cloneAnyMap(value.Meta)
		value.Headers = cloneHTTPHeaders(value.Headers)

		return acp.McpServer{Sse: &value}
	case server.Acp != nil:
		value := *server.Acp
		value.Meta = cloneAnyMap(value.Meta)

		return acp.McpServer{Acp: &value}
	case server.Stdio != nil:
		value := cloneMCPServerStdio(server.Stdio)

		return acp.McpServer{Stdio: value}
	default:
		return acp.McpServer{}
	}
}

func cloneMCPServerStdio(server *acp.McpServerStdio) *acp.McpServerStdio {
	if server == nil {
		return nil
	}

	value := *server
	value.Meta = cloneAnyMap(value.Meta)
	value.Args = append([]string(nil), value.Args...)
	value.Env = cloneEnvVariables(value.Env)

	return &value
}

func cloneHTTPHeaders(headers []acp.HttpHeader) []acp.HttpHeader {
	if headers == nil {
		return nil
	}

	cloned := make([]acp.HttpHeader, len(headers))
	for index, header := range headers {
		cloned[index] = header
		cloned[index].Meta = cloneAnyMap(header.Meta)
	}

	return cloned
}

func cloneEnvVariables(env []acp.EnvVariable) []acp.EnvVariable {
	if env == nil {
		return nil
	}

	cloned := make([]acp.EnvVariable, len(env))
	for index, variable := range env {
		cloned[index] = variable
		cloned[index].Meta = cloneAnyMap(variable.Meta)
	}

	return cloned
}

func unstableMCPServersFromStable(servers []acp.McpServer) []acp.UnstableMcpServer {
	if servers == nil {
		return nil
	}

	cloned := make([]acp.UnstableMcpServer, len(servers))
	for index, server := range servers {
		cloned[index] = unstableMCPServerFromStable(server)
	}

	return cloned
}

func unstableMCPServerFromStable(server acp.McpServer) acp.UnstableMcpServer {
	switch {
	case server.Http != nil:
		value := acp.UnstableMcpServerHttp{
			Meta:    cloneAnyMap(server.Http.Meta),
			Headers: cloneHTTPHeaders(server.Http.Headers),
			Name:    server.Http.Name,
			Type:    server.Http.Type,
			Url:     server.Http.Url,
		}

		return acp.UnstableMcpServer{Http: &value}
	case server.Sse != nil:
		value := acp.UnstableMcpServerSse{
			Meta:    cloneAnyMap(server.Sse.Meta),
			Headers: cloneHTTPHeaders(server.Sse.Headers),
			Name:    server.Sse.Name,
			Type:    server.Sse.Type,
			Url:     server.Sse.Url,
		}

		return acp.UnstableMcpServer{Sse: &value}
	case server.Acp != nil:
		value := acp.UnstableMcpServerAcpInline{
			Meta: cloneAnyMap(server.Acp.Meta),
			Id:   acp.UnstableMcpServerAcpId(server.Acp.Id),
			Name: server.Acp.Name,
			Type: server.Acp.Type,
		}

		return acp.UnstableMcpServer{Acp: &value}
	case server.Stdio != nil:
		return acp.UnstableMcpServer{Stdio: cloneMCPServerStdio(server.Stdio)}
	default:
		return acp.UnstableMcpServer{}
	}
}
