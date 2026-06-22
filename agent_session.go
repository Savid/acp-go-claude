package claudeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/observer"
	"github.com/savid/acp-go-claude/internal/permissions"
	"github.com/savid/acp-go-claude/internal/transcript"
)

// NewSession creates and starts a Claude CLI session.
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (resp acp.NewSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/new")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	goalInput, err := parseGoalFromMeta(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories, metaOptions)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.NewSessionResponse{}, validationErr
	}

	sessionID, err := newUUID()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.NewSessionResponse{}, openErr
	}

	session, err := a.startSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            params.McpServers,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	})
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	if err := a.storeStartedSession(ctx, session); err != nil {
		return acp.NewSessionResponse{}, err
	}

	session.applyStoredClientGoalInput(goalInput)

	if err := session.emitOptionalUpdates(ctx, mapper.AvailableCommandsUpdate(session.commands())); err != nil {
		a.removeSession(ctx, session.id, session)

		return acp.NewSessionResponse{}, err
	}

	resp = acp.NewSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionConfigOptions(session),
	}

	return resp, nil
}

// ResumeSession resumes a Claude session without replaying previous updates.
func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (resp acp.ResumeSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/resume")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	goalInput, err := parseGoalFromMeta(params.Meta)
	if err != nil {
		return acp.ResumeSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories, metaOptions)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.ResumeSessionResponse{}, validationErr
	}

	start := sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            params.McpServers,
		ResumeID:              string(params.SessionId),
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	}
	if session := a.activeSessionForStart(params.SessionId, start); session != nil {
		session.applyStoredClientGoalInput(goalInput)

		if emitErr := session.emitOptionalUpdates(ctx, mapper.AvailableCommandsUpdate(session.commands())); emitErr != nil {
			return acp.ResumeSessionResponse{}, emitErr
		}

		session.emitCurrentUsageUpdate(ctx)

		resp = acp.ResumeSessionResponse{
			Meta:          sessionResponseMeta(session),
			ConfigOptions: sessionConfigOptions(session),
		}

		return resp, nil
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.ResumeSessionResponse{}, openErr
	}

	session, err := a.startSession(ctx, params.SessionId, start)
	if err != nil {
		if missingClaudeSessionError(err) {
			return acp.ResumeSessionResponse{}, newResourceNotFound(map[string]any{acpFieldSessionID: params.SessionId})
		}

		return acp.ResumeSessionResponse{}, err
	}

	if err := a.storeStartedSession(ctx, session); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	session.applyStoredClientGoalInput(goalInput)

	if err := session.emitOptionalUpdates(ctx, mapper.AvailableCommandsUpdate(session.commands())); err != nil {
		a.removeSession(ctx, params.SessionId, session)

		return acp.ResumeSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	resp = acp.ResumeSessionResponse{
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionConfigOptions(session),
	}

	return resp, nil
}

// LoadSession resumes a Claude session and replays saved transcript updates when available.
func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (resp acp.LoadSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/load")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	goalInput, err := parseGoalFromMeta(params.Meta)
	if err != nil {
		return acp.LoadSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories, metaOptions)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.LoadSessionResponse{}, validationErr
	}

	saved, err := transcript.Store{ClaudeHome: a.options.ClaudeHome}.Find(ctx, string(params.SessionId), params.Cwd)

	savedPath := ""
	if err == nil {
		savedPath = saved.Path
	} else {
		if errors.Is(err, os.ErrNotExist) && !a.storeHasSession(ctx, string(params.SessionId), params.Cwd) {
			return acp.LoadSessionResponse{}, newResourceNotFound(map[string]any{acpFieldSessionID: params.SessionId})
		}

		if !errors.Is(err, os.ErrNotExist) {
			return acp.LoadSessionResponse{}, err
		}
	}

	start := sessionStart{
		Cwd:                   params.Cwd,
		McpServers:            params.McpServers,
		AdditionalDirectories: additionalDirectories,
		ResumeID:              string(params.SessionId),
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	}

	session := a.activeSessionForStart(params.SessionId, start)
	startedSession := false

	if session == nil {
		if openErr := a.ensureOpen(); openErr != nil {
			return acp.LoadSessionResponse{}, openErr
		}

		session, err = a.startSession(ctx, params.SessionId, start)
		if err != nil {
			if missingClaudeSessionError(err) {
				return acp.LoadSessionResponse{}, newResourceNotFound(map[string]any{acpFieldSessionID: params.SessionId})
			}

			return acp.LoadSessionResponse{}, err
		}

		if storeErr := a.storeStartedSession(ctx, session); storeErr != nil {
			return acp.LoadSessionResponse{}, storeErr
		}

		startedSession = true
	}

	replayPath := savedPath
	if session.materialized != nil && session.materialized.mainPath != "" {
		replayPath = session.materialized.mainPath
	}

	if replayPath == "" {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, newResourceNotFound(map[string]any{acpFieldSessionID: params.SessionId})
	}

	if replayErr := session.replayTranscript(ctx, replayPath); replayErr != nil {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, replayErr
	}

	goalChanged, err := session.applyReplayGoalSnapshot(ctx, replayPath)
	if err != nil {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, err
	}

	if session.applyStoredClientGoalInput(goalInput) {
		goalChanged = true
	}

	if goalChanged {
		if err := session.emitGoalInfoUpdate(ctx); err != nil {
			if startedSession {
				a.removeSession(ctx, params.SessionId, session)
			}

			return acp.LoadSessionResponse{}, err
		}
	}

	if err := session.emitOptionalUpdates(ctx, mapper.AvailableCommandsUpdate(session.commands())); err != nil {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	resp = acp.LoadSessionResponse{
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionConfigOptions(session),
	}

	return resp, nil
}

// ListSessions lists active sessions and saved Claude transcript sessions.
func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (resp acp.ListSessionsResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/list")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	if validationErr := validateOptionalAbsolutePath("cwd", params.Cwd); validationErr != nil {
		return acp.ListSessionsResponse{}, validationErr
	}

	a.mu.Lock()

	activeSessions := make(map[acp.SessionId]*Session, len(a.sessions))
	for id, session := range a.sessions {
		if !sessionMatchesListFilters(session, params) {
			continue
		}

		activeSessions[id] = session
	}
	a.mu.Unlock()

	active := make([]acp.SessionInfo, 0, len(activeSessions))
	for id, session := range activeSessions {
		active = append(active, session.sessionInfo(id))
	}

	saved, err := transcript.Store{ClaudeHome: a.options.ClaudeHome}.List(ctx, params.Cwd, nil)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	storeSessions, err := a.listStoreSessions(ctx, params)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	sessions := make([]acp.SessionInfo, 0, len(active)+len(saved)+len(storeSessions))
	seen := make(map[acp.SessionId]struct{}, len(active)+len(saved)+len(storeSessions))

	for _, session := range active {
		sessions = append(sessions, session)
		seen[session.SessionId] = struct{}{}
	}

	for _, session := range saved {
		if _, ok := seen[session.Info.SessionId]; ok {
			continue
		}

		sessions = append(sessions, session.Info)
		seen[session.Info.SessionId] = struct{}{}
	}

	for _, session := range storeSessions {
		if _, ok := seen[session.SessionId]; ok {
			continue
		}

		sessions = append(sessions, session)
		seen[session.SessionId] = struct{}{}
	}

	paged, nextCursor, err := paginateSessionInfos(sessions, params.Cursor)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	return acp.ListSessionsResponse{Sessions: paged, NextCursor: nextCursor}, nil
}

// Prompt sends a user prompt to Claude and streams ACP session updates until the turn ends.
func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (resp acp.PromptResponse, err error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	ctx, finish := a.observe.StartPrompt(ctx, params.Meta, session.currentModel())
	defer func() { finish(promptResultForObserver(resp, err, session.currentModel())) }()

	resp, err = session.Prompt(ctx, params)
	if err != nil && fatalClaudeProcessError(err) {
		a.removeSession(ctx, params.SessionId, session)
		a.observe.RecordClaudeProcessExit(ctx, "unexpected", err)

		err = acp.NewInternalError(map[string]any{
			jsonFieldError:   err.Error(),
			jsonFieldMessage: "The Claude Agent process exited unexpectedly. Please start a new session.",
		})

		return acp.PromptResponse{}, err
	}

	return resp, err
}

// Cancel interrupts an active Claude turn for the session.
func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) (err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/cancel")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	session, err := a.session(params.SessionId)
	if err != nil {
		return err
	}

	err = session.Cancel(ctx)

	return err
}

// CloseSession closes a Claude session process and removes it from the active map.
func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (resp acp.CloseSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/close")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}

	_ = session.Cancel(ctx)
	closeErr := session.Close(ctx)

	a.mu.Lock()
	_, existed := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.deleteCachedPermissionRulesLocked(params.SessionId)
	a.mu.Unlock()

	if existed {
		a.observe.AddActiveSession(ctx, -1)
	}

	a.docsMu.Lock()
	delete(a.documents, params.SessionId)
	delete(a.focusedDocuments, params.SessionId)
	a.docsMu.Unlock()

	if closeErr != nil {
		return acp.CloseSessionResponse{}, closeErr
	}

	return acp.CloseSessionResponse{}, nil
}

func (a *Agent) session(sessionID acp.SessionId) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[sessionID]
	if session == nil {
		return nil, acp.NewInvalidParams(map[string]any{acpFieldSessionID: sessionID})
	}

	return session, nil
}

func (s *Session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.model
}

func promptResultForObserver(resp acp.PromptResponse, err error, model string) observer.PromptResult {
	result := observer.PromptResult{
		Err:        err,
		Model:      model,
		StopReason: string(resp.StopReason),
	}
	if resp.Usage == nil {
		return result
	}

	result.InputTokens = resp.Usage.InputTokens
	result.OutputTokens = resp.Usage.OutputTokens
	result.TotalTokens = resp.Usage.TotalTokens

	if resp.Usage.CachedReadTokens != nil {
		result.CachedReadTokens = *resp.Usage.CachedReadTokens
	}

	if resp.Usage.CachedWriteTokens != nil {
		result.CachedWriteTokens = *resp.Usage.CachedWriteTokens
	}

	if resp.Usage.ThoughtTokens != nil {
		result.ThoughtTokens = *resp.Usage.ThoughtTokens
	}

	return result
}

func (a *Agent) removeSession(ctx context.Context, sessionID acp.SessionId, session *Session) {
	a.mu.Lock()
	removed := false

	if a.sessions[sessionID] == session {
		delete(a.sessions, sessionID)
		a.deleteCachedPermissionRulesLocked(sessionID)

		removed = true
	}

	a.mu.Unlock()

	if removed {
		a.observe.AddActiveSession(context.Background(), -1)
	}

	if session != nil {
		if err := session.Close(ctx); err != nil {
			a.log.DebugContext(ctx, "close removed Claude session failed", slog.String(jsonFieldError, err.Error()))
		}
	}
}

func (a *Agent) ensureOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return errAgentClosed
	}

	return nil
}

func (a *Agent) storeStartedSession(ctx context.Context, session *Session) error {
	a.mu.Lock()
	if a.closed {
		a.deleteCachedPermissionRulesLocked(session.id)
		a.mu.Unlock()

		if err := session.Close(ctx); err != nil {
			a.log.DebugContext(ctx, "close rejected Claude session failed", slog.String(jsonFieldError, err.Error()))
		}

		return errAgentClosed
	}

	previous := a.sessions[session.id]
	a.sessions[session.id] = session
	a.mu.Unlock()

	if previous != nil {
		if err := previous.Close(ctx); err != nil {
			a.log.WarnContext(ctx, "close replaced Claude session failed", slog.String(jsonFieldError, err.Error()))
		}

		return nil
	}

	a.observe.AddActiveSession(ctx, 1)

	return nil
}

func (a *Agent) connection() agentClient {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.conn
}

func (a *Agent) clientElicitationCapabilities() *acp.ElicitationCapabilities {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.clientCapabilities.Elicitation
}

func (a *Agent) clientSupportsTerminalOutput() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return clientMetaBool(a.clientCapabilities.Meta, clientMetaTerminalOutput)
}

func (a *Agent) activeSessionForStart(id acp.SessionId, start sessionStart) *Session {
	fingerprint := sessionStartFingerprint(start)

	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[id]
	if session == nil || session.fingerprint != fingerprint {
		return nil
	}

	return session
}

func sessionStartFingerprint(start sessionStart) string {
	servers := slices.Clone(start.McpServers)
	slices.SortFunc(servers, func(left, right acp.McpServer) int {
		return strings.Compare(mcpServerName(left), mcpServerName(right))
	})

	data := struct {
		Cwd                   string           `json:"cwd"`
		AdditionalDirectories []string         `json:"additionalDirectories,omitempty"`
		McpServers            []acp.McpServer  `json:"mcpServers,omitempty"`
		MetaOptions           ClaudeOptions    `json:"metaOptions,omitzero"`
		RawMessages           rawMessageConfig `json:"rawMessages,omitzero"`
	}{
		Cwd:                   start.Cwd,
		AdditionalDirectories: slices.Clone(start.AdditionalDirectories),
		McpServers:            servers,
		MetaOptions:           start.MetaOptions,
		RawMessages:           start.RawMessages,
	}

	return jsonFingerprint(data)
}

func jsonFingerprint(data any) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Sprintf("marshal-error:%T:%v", data, err)
	}

	return string(encoded)
}

func mcpServerName(server acp.McpServer) string {
	switch {
	case server.Http != nil:
		return server.Http.Name
	case server.Sse != nil:
		return server.Sse.Name
	case server.Acp != nil:
		return server.Acp.Name
	case server.Stdio != nil:
		return server.Stdio.Name
	default:
		return ""
	}
}

func newResourceNotFound(data any) *acp.RequestError {
	return &acp.RequestError{Code: -32002, Message: "Resource not found", Data: data}
}

func missingClaudeSessionError(err error) bool {
	return errors.Is(err, claude.ErrSessionNotFound) || errors.Is(err, claude.ErrQueryClosed)
}

func (a *Agent) startSession(ctx context.Context, id acp.SessionId, start sessionStart) (*Session, error) {
	gatewayEnv := a.gatewayEnv()

	if err := a.validateGatewayMCPIsolation(start, gatewayEnv != nil); err != nil {
		return nil, err
	}

	claudeHome, err := canonicalClaudeHome(a.options.ClaudeHome)
	if err != nil {
		return nil, err
	}

	mcpConfig, mcpBridge, err := a.mcpConfigForStart(ctx, id, start)
	if err != nil {
		return nil, err
	}

	discoverCtx, finishDiscover := a.observe.StartClaudeProcess(ctx, "discover")
	discoveredSettings := loadDiscoveredSettings(discoverCtx, start.Cwd, claudeHome, a.log)

	finishDiscover(nil)

	env := mergeEnv(discoveredSettings.Env, a.options.Env)
	env = mergeEnv(env, start.MetaOptions.Env)
	env = mergeEnv(env, gatewayEnv)
	env = a.observe.InjectTraceEnv(ctx, env)

	materialized, err := a.materializeStoreSession(ctx, start.ResumeID, start.Cwd, claudeHome, env)
	if err != nil {
		closeSessionStartResources(mcpBridge, nil)

		return nil, err
	}

	processClaudeHome := claudeHome
	if materialized != nil {
		processClaudeHome = materialized.configDir
	}

	modelConfig, hasModelConfig, err := modelConfigFromEnv(env)
	if err != nil {
		closeSessionStartResources(mcpBridge, materialized)

		return nil, err
	}

	modelOverrides := map[string]string(nil)
	if hasModelConfig {
		modelOverrides = modelConfig.ModelOverrides
	}

	permissionMode := a.options.DefaultPermissionMode
	if !a.options.defaultPermissionModeSet && discoveredSettings.PermissionMode != "" {
		permissionMode = discoveredSettings.PermissionMode
	}

	if start.MetaOptions.PermissionMode != "" {
		permissionMode = start.MetaOptions.PermissionMode
	}

	if acpModeForPermission(permissionMode) == modeBypassPermissions && !bypassPermissionsAvailable() {
		permissionMode = string(modeDefault)
	}

	defaultModel := firstNonEmptyString(start.MetaOptions.Model, a.options.DefaultModel)

	options := claude.Options{
		CLIPath:                 a.options.ClaudePath,
		Cwd:                     start.Cwd,
		ClaudeHome:              processClaudeHome,
		Env:                     env,
		SessionID:               string(id),
		ResumeID:                start.ResumeID,
		ForkSession:             start.ForkSession,
		Bare:                    a.options.BareMode || start.MetaOptions.Bare,
		Model:                   claudeModelID(defaultModel, modelOverrides),
		SystemText:              firstNonEmptyString(start.MetaOptions.SystemPrompt, a.options.DefaultSystemPrompt),
		JSONSchema:              outputFormatJSONSchema(start.MetaOptions.OutputFormat),
		PermissionMode:          permissionMode,
		PermissionPromptTool:    permissionPromptTool,
		AllowSkipPermissionsArg: a.options.AllowSkipPermissionsFlag && bypassPermissionsAvailable(),
		AddDirs:                 start.AdditionalDirectories,
		MCPConfigJSON:           mcpConfig,
		SettingSources:          settingSourceArgs(a.options.SettingSources),
		InitializeTimeout:       a.options.InitializeTimeout,
		ControlHandlerTimeout:   a.options.ControlHandlerTimeout,
		SessionMirror:           true,
		Hooks: claude.Hooks{
			claude.HookEventPostToolUse: {
				{
					Matcher:         "*",
					HookCallbackIDs: []string{systemHookCallbackID},
					TimeoutSeconds:  30,
				},
			},
		},
	}

	permissionRules, err := a.permissionRulesForStart(ctx, id, start)
	if err != nil {
		closeSessionStartResources(mcpBridge, materialized)

		return nil, err
	}

	session := &Session{
		agent:                 a,
		id:                    id,
		turn:                  make(chan struct{}, 1),
		cwd:                   start.Cwd,
		additionalDirectories: slices.Clone(start.AdditionalDirectories),
		fingerprint:           sessionStartFingerprint(start),
		model:                 defaultModel,
		modelOverrides:        cloneStringMap(modelOverrides),
		mode:                  acpModeForPermission(permissionMode),
		permissionRules:       permissions.Clone(permissionRules),
		mcpBridge:             mcpBridge,
		materialized:          materialized,
		mirror:                newSessionMirror(a.log, a.options.SessionStore, processClaudeHome),
		rawMessages:           start.RawMessages,
		gatewayAuth:           gatewayEnv != nil,
	}
	options.PermissionHandler = session.handlePermission
	options.ElicitationHandler = session.handleElicitation
	options.HookHandler = session.handleHookCallback
	session.client = a.newClaudeClient(a.log, options)

	startCtx, finishStart := a.observe.StartClaudeProcess(ctx, "start")
	startErr := session.client.Start(startCtx)
	finishStart(startErr)

	if startErr != nil {
		closeSessionStartResources(mcpBridge, materialized)

		return nil, startErr
	}

	started := true
	defer func() {
		if started {
			_ = session.client.Close()

			closeSessionStartResources(mcpBridge, materialized)
		}
	}()

	info := session.client.InitializeInfo()
	availableModels := info.Models

	availableModelAllowlist, hasAvailableModelAllowlist := settingsAvailableModelAllowlist(modelConfig, hasModelConfig, discoveredSettings)
	if hasAvailableModelAllowlist {
		availableModels = applyAvailableModelsAllowlist(availableModels, availableModelAllowlist)
	}

	initCtx, finishInitialize := a.observe.StartClaudeProcess(ctx, "initialize")
	settings, err := session.client.GetSettings(initCtx)
	finishInitialize(err)

	settingsKnown := true

	if err != nil {
		a.log.DebugContext(ctx, "get Claude settings failed", slog.String(jsonFieldError, err.Error()))

		settings = &claude.SettingsSnapshot{}
		settingsKnown = false
	}

	selectedModel := selectInitialModel(
		defaultModel,
		envValue(env, envAnthropicModel),
		firstNonEmptyString(settings.Applied.Model, discoveredSettings.Model),
		availableModels,
	)
	if selectedModel.ShouldApply {
		if err := session.client.SetModel(ctx, claudeModelID(selectedModel.Model, modelOverrides)); err != nil {
			return nil, err
		}
	}

	session.mu.Lock()
	session.model = selectedModel.Model
	session.availableModels = availableModels
	session.availableCommands = info.Commands
	session.outputStyle = info.OutputStyle
	session.availableOutputStyles = info.AvailableOutputStyles
	session.effort = firstNonEmptyString(settings.Applied.Effort, discoveredSettings.Effort)

	if effort, changed := reconcileEffortForModel(selectedModel.Model, availableModels, session.effort); changed {
		session.effort = effort
	}

	session.contextWindowSize = contextWindowForAvailableModel(selectedModel.Model, availableModels)

	if settings.FastMode != nil {
		session.fastMode = *settings.FastMode
	}

	session.fastModeKnown = settingsKnown

	if !modeAvailableForModel(session.mode, session.model, session.availableModels) {
		session.mode = modeDefault
	}
	session.mu.Unlock()

	if session.mode == modeDefault && acpModeForPermission(permissionMode) != modeDefault {
		if err := session.client.SetPermissionMode(ctx, string(modeDefault)); err != nil {
			return nil, err
		}
	}

	started = false

	return session, nil
}

func closeSessionStartResources(mcpBridge *mcpSessionBridge, materialized *materializedSession) {
	if mcpBridge != nil {
		mcpBridge.Close()
	}

	if materialized != nil {
		_ = materialized.Close()
	}
}

func (a *Agent) mcpConfigForStart(
	ctx context.Context,
	id acp.SessionId,
	start sessionStart,
) (string, *mcpSessionBridge, error) {
	mcpServers, mcpBridge, err := a.prepareMCPServers(ctx, id, start.McpServers)
	if err != nil {
		return "", nil, err
	}

	a.warnDeprecatedSSEMCPServers(ctx, "mcp_servers", sseMCPServerNames(mcpServers))

	mcpConfig, err := mapper.MCPServersToClaude(mcpServers)
	if err != nil {
		if mcpBridge != nil {
			mcpBridge.Close()
		}

		return "", nil, err
	}

	return mcpConfig, mcpBridge, nil
}

func (a *Agent) warnDeprecatedSSEMCPServers(ctx context.Context, source string, names []string) {
	if len(names) == 0 {
		return
	}

	for _, name := range names {
		a.log.WarnContext(
			ctx,
			"SSE MCP transport is deprecated; prefer HTTP MCP transport",
			slog.String("server", name),
			slog.String("source", source),
		)
	}
}

func sseMCPServerNames(servers []acp.McpServer) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		if server.Sse != nil {
			names = append(names, server.Sse.Name)
		}
	}

	return names
}

func (a *Agent) validateGatewayMCPIsolation(start sessionStart, gatewayAuth bool) error {
	if !gatewayAuth {
		return nil
	}

	if mcpServersUseProcess(start.McpServers) {
		return fmt.Errorf("gateway auth cannot be used with stdio or ACP MCP servers because Claude-launched MCP processes inherit gateway credentials")
	}

	return nil
}

func mcpServersUseProcess(servers []acp.McpServer) bool {
	for _, server := range servers {
		if server.Stdio != nil || server.Acp != nil {
			return true
		}
	}

	return false
}

func (a *Agent) storeHasSession(ctx context.Context, sessionID string, cwd string) bool {
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		return false
	}

	entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{ProjectKey: projectKey, SessionID: sessionID})

	return err == nil && len(entries) > 0
}

func (a *Agent) listStoreSessions(ctx context.Context, params acp.ListSessionsRequest) ([]acp.SessionInfo, error) {
	if params.Cwd == nil || strings.TrimSpace(*params.Cwd) == "" {
		return nil, nil
	}

	lister, ok := a.sessionStore().(SessionStoreLister)
	if !ok {
		return nil, nil
	}

	projectKey, err := projectKeyForDirectory(*params.Cwd)
	if err != nil {
		return nil, err
	}

	listCtx, cancel := context.WithTimeout(ctx, a.sessionStoreLoadTimeout())
	defer cancel()

	listCtx, finishList := a.observe.StartSessionStore(listCtx, "list")
	summaries, err := lister.ListSessions(listCtx, projectKey)
	finishList(err)

	if err != nil {
		return nil, fmt.Errorf("list session store: %w", err)
	}

	infos := make([]acp.SessionInfo, 0, len(summaries))
	for _, summary := range summaries {
		if !validUUIDShape(summary.SessionID) {
			continue
		}

		entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{ProjectKey: projectKey, SessionID: summary.SessionID})
		if err != nil {
			return nil, err
		}

		title := storeSessionTitle(summary.SessionID, entries)
		updatedAt := time.UnixMilli(summary.MTime).UTC().Format(time.RFC3339)
		infos = append(infos, acp.SessionInfo{
			SessionId: acp.SessionId(summary.SessionID),
			Cwd:       *params.Cwd,
			Title:     &title,
			UpdatedAt: &updatedAt,
		})
	}

	return infos, nil
}

func storeSessionTitle(sessionID string, entries []SessionStoreEntry) string {
	for _, entry := range entries {
		var obj map[string]any
		if json.Unmarshal(entry, &obj) != nil {
			continue
		}

		if title, _ := obj["aiTitle"].(string); strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title)
		}

		if title, _ := obj["customTitle"].(string); strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title)
		}

		if title := firstStoreUserPrompt(obj); title != "" {
			return title
		}
	}

	return sessionID
}

func firstStoreUserPrompt(entry map[string]any) string {
	if entry[jsonFieldType] != claude.MessageTypeUser {
		return ""
	}

	message, _ := entry["message"].(map[string]any)
	content := message["content"]

	if text, _ := content.(string); strings.TrimSpace(text) != "" {
		return normalizeLiveSessionTitle(text)
	}

	values, _ := content.([]any)
	for _, value := range values {
		block, _ := value.(map[string]any)
		if block[jsonFieldType] != claude.BlockTypeText {
			continue
		}

		if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
			return normalizeLiveSessionTitle(text)
		}
	}

	return ""
}

func sessionMatchesListFilters(session *Session, params acp.ListSessionsRequest) bool {
	if params.Cwd != nil && *params.Cwd != session.cwd {
		return false
	}

	return true
}

func paginateSessionInfos(sessions []acp.SessionInfo, cursor *string) ([]acp.SessionInfo, *string, error) {
	offset, err := decodeListCursor(cursor)
	if err != nil {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "invalid cursor"})
	}

	if offset > len(sessions) {
		return nil, nil, acp.NewInvalidParams(map[string]any{"cursor": "cursor is past end"})
	}

	end := offset + listSessionsPageSize
	if end >= len(sessions) {
		return sessions[offset:], nil, nil
	}

	next := encodeListCursor(end)

	return sessions[offset:end], &next, nil
}

func decodeListCursor(cursor *string) (int, error) {
	if cursor == nil || *cursor == "" {
		return 0, nil
	}

	data, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return 0, err
	}

	offset, err := strconv.Atoi(string(data))
	if err != nil || offset < 0 {
		return 0, strconv.ErrSyntax
	}

	return offset, nil
}

func encodeListCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}
