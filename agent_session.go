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

var mapMCPServersToClaude = mapper.MCPServersToClaude

// NewSession creates and starts a Claude CLI session.
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (resp acp.NewSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/new")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, lifecycleMetaError(err)
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
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
		return acp.ResumeSessionResponse{}, lifecycleMetaError(err)
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
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
		session.emitCurrentUsageUpdate(ctx)

		resp = acp.ResumeSessionResponse{
			Meta:          sessionResponseMeta(session),
			ConfigOptions: sessionConfigOptions(session),
		}

		return resp, nil
	}

	if blocked, blockErr := a.nativeSessionBlocked(ctx, params.SessionId); blockErr != nil {
		return acp.ResumeSessionResponse{}, blockErr
	} else if blocked {
		return acp.ResumeSessionResponse{}, unknownSessionError()
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.ResumeSessionResponse{}, openErr
	}

	session, err := a.startSession(ctx, params.SessionId, start)
	if err != nil {
		if missingClaudeSessionError(err) {
			return acp.ResumeSessionResponse{}, unknownSessionError()
		}

		return acp.ResumeSessionResponse{}, err
	}

	if err := a.storeStartedSession(ctx, session); err != nil {
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
		return acp.LoadSessionResponse{}, lifecycleMetaError(err)
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.LoadSessionResponse{}, validationErr
	}

	if blocked, blockErr := a.nativeSessionBlocked(ctx, params.SessionId); blockErr != nil {
		return acp.LoadSessionResponse{}, blockErr
	} else if blocked {
		return acp.LoadSessionResponse{}, unknownSessionError()
	}

	saved, err := transcript.Store{ClaudeHome: a.options.Home}.Find(ctx, string(params.SessionId), params.Cwd)

	savedPath := ""
	if err == nil {
		savedPath = saved.Path
	} else {
		if errors.Is(err, os.ErrNotExist) && !a.storeHasSession(ctx, string(params.SessionId), params.Cwd) {
			return acp.LoadSessionResponse{}, unknownSessionError()
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
				return acp.LoadSessionResponse{}, unknownSessionError()
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

		return acp.LoadSessionResponse{}, unknownSessionError()
	}

	if replayErr := session.replayTranscript(ctx, replayPath); replayErr != nil {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, replayErr
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

	activeSessions := make(map[acp.SessionId]*agentSession, len(a.sessions))
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

	a.retryDeletedNativeTranscripts(ctx)

	var saved []transcript.Session
	if a.nativeSessionFallbackEnabled() {
		saved, err = transcript.Store{ClaudeHome: a.options.Home}.List(ctx, params.Cwd, nil)
		if err != nil {
			return acp.ListSessionsResponse{}, err
		}
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
		if a.isDeleted(session.Info.SessionId) {
			a.retryDeleteNativeTranscript(ctx, session.Info.SessionId)

			continue
		}

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

	_, err = parseInboundTurnRoute(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	ctx, finish := a.observe.StartPrompt(ctx, params.Meta, session.currentModel())
	defer func() { finish(promptResultForObserver(resp, err, session.currentModel())) }()

	// A native turn failure (process death, transport, provider, timeout) leaves
	// the session addressable and retriable: it is not removed from the map, so a
	// follow-up session/prompt relaunches the native process lazily rather than
	// returning the unknown-session error. session.Prompt already maps the real
	// cause into the uniform claude_turn_failed error.
	resp, err = session.Prompt(ctx, params)

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

	err = session.cancelRouted(ctx, params.Meta)

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

	if closeErr != nil {
		return acp.CloseSessionResponse{}, closeErr
	}

	return acp.CloseSessionResponse{}, nil
}

// UnstableDeleteSession implements ACP session/delete.
func (a *Agent) UnstableDeleteSession(
	ctx context.Context,
	params acp.UnstableDeleteSessionRequest,
) (acp.UnstableDeleteSessionResponse, error) {
	if err := a.sessionStore().Delete(ctx, SessionKey{SessionID: string(params.SessionId)}); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	a.mu.Lock()
	session := a.sessions[params.SessionId]
	delete(a.sessions, params.SessionId)
	a.deleted[params.SessionId] = struct{}{}
	a.deleteCachedPermissionRulesLocked(params.SessionId)
	a.mu.Unlock()

	var cleanupErr error

	if session != nil {
		_ = session.Cancel(ctx)
		if err := session.Close(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}

		a.observe.AddActiveSession(ctx, -1)
	}

	if err := deleteNativeTranscript(ctx, a.options.Home, string(params.SessionId)); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}

	if cleanupErr != nil {
		return acp.UnstableDeleteSessionResponse{}, cleanupErr
	}

	return acp.UnstableDeleteSessionResponse{}, nil
}

func (a *Agent) session(sessionID acp.SessionId) (*agentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	session := a.sessions[sessionID]
	if session == nil {
		return nil, unknownSessionError()
	}

	return session, nil
}

func (s *agentSession) currentModel() string {
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

func (a *Agent) removeSession(ctx context.Context, sessionID acp.SessionId, session *agentSession) {
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

func (a *Agent) storeStartedSession(ctx context.Context, session *agentSession) error {
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
	if previous == nil && len(a.sessions) >= a.maxActiveSessions() {
		a.mu.Unlock()

		if err := session.Close(ctx); err != nil {
			a.log.DebugContext(ctx, "close backpressured Claude session failed", slog.String(jsonFieldError, err.Error()))
		}

		return backpressureError("active_sessions")
	}

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

func (a *Agent) clientSupportsFormElicitation() bool {
	caps := a.clientElicitationCapabilities()
	if caps == nil {
		return false
	}

	return caps.Form != nil || caps.Url == nil
}

func (a *Agent) clientSupportsURLElicitation() bool {
	caps := a.clientElicitationCapabilities()

	return caps != nil && caps.Url != nil
}

func (a *Agent) clientSupportsTerminalOutput() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return clientMetaBool(a.clientCapabilities.Meta, clientMetaTerminalOutput)
}

func (a *Agent) activeSessionForStart(id acp.SessionId, start sessionStart) *agentSession {
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

// unknownSessionError is returned by every session-scoped method when the
// session id cannot be resolved (unknown, not in the store, tombstoned, or its
// native transcript is gone). All such cases share one invalid-params shape.
func unknownSessionError() *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: "unknown session",
		jsonFieldField: acpFieldSessionID,
	})
}

func missingClaudeSessionError(err error) bool {
	return errors.Is(err, claude.ErrSessionNotFound) || errors.Is(err, claude.ErrQueryClosed)
}

func (a *Agent) startSession(ctx context.Context, id acp.SessionId, start sessionStart) (_ *agentSession, err error) { //nolint:gocyclo // Session startup owns the complete resource unwind graph.
	scratchRelease, err := reserveScratchRoot(ctx, a.options.RuntimeResourceHooks, RuntimeResourceSession)
	if err != nil {
		return nil, err
	}

	var (
		materialized  *materializedSession
		mcpConfigDir  string
		nativeRelease func()
		cleanupClient *claude.Client
	)

	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}

		var closeErr error
		if cleanupClient != nil {
			closeErr = cleanupClient.Close()
		}

		err = finalizeSessionRuntimeResources(
			errors.Join(err, closeErr), nativeRelease, mcpConfigDir, materialized, scratchRelease,
		)
	}()

	claudeHome, err := canonicalClaudeHome(a.options.Home)
	if err != nil {
		return nil, err
	}

	mcpConfig, err := a.mcpConfigForStart(start)
	if err != nil {
		return nil, err
	}

	discoverCtx, finishDiscover := a.observe.StartClaudeProcess(ctx, "discover")
	discoveredSettings := loadDiscoveredSettings(discoverCtx, start.Cwd, claudeHome, a.log)

	finishDiscover(nil)

	env := mergeEnv(discoveredSettings.Env, a.options.Env)
	env = mergeEnv(env, start.MetaOptions.Env)
	env = a.observe.InjectTraceEnv(ctx, env)

	materialized, err = a.materializeStoreSession(ctx, start.ResumeID, start.Cwd, claudeHome, env)
	if err != nil {
		return nil, err
	}

	processClaudeHome := claudeHome
	if materialized != nil {
		processClaudeHome = materialized.configDir
	}

	settingsFileArg, err := a.prepareSeededClaudeConfig(claudeHome, processClaudeHome)
	if err != nil {
		return nil, err
	}

	configurationStarted := time.Now()
	mcpConfigPath, mcpConfigDir, err := writeSessionMCPConfig(a.options.ScratchDir, mcpConfig)
	observeRuntimeStartupStage(ctx, a.options.RuntimeResourceHooks, RuntimeResourceSession, RuntimeStartupConfiguration, configurationStarted, err)

	if err != nil {
		return nil, err
	}

	modelConfig, hasModelConfig, err := modelConfigFromEnv(env)
	if err != nil {
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
		CLIPath:                 a.options.ExecutablePath,
		Cwd:                     start.Cwd,
		ClaudeHome:              processClaudeHome,
		Env:                     env,
		SessionID:               string(id),
		ResumeID:                start.ResumeID,
		ForkSession:             start.ForkSession,
		Bare:                    a.options.BareMode || start.MetaOptions.Bare,
		Model:                   claudeModelID(defaultModel, modelOverrides),
		SystemText:              firstNonEmptyString(start.MetaOptions.SystemPrompt, a.options.DefaultSystemPrompt),
		JSONSchema:              outputSchemaJSONSchema(start.MetaOptions.OutputSchema),
		PermissionMode:          permissionMode,
		PermissionPromptTool:    permissionPromptTool,
		AllowSkipPermissionsArg: a.options.AllowSkipPermissionsFlag && bypassPermissionsAvailable(),
		AddDirs:                 start.AdditionalDirectories,
		MCPConfigPath:           mcpConfigPath,
		SettingSources:          settingSourceArgs(a.options.SettingSources),
		SettingsFile:            settingsFileArg,
		InitializeTimeout:       a.options.InitializeTimeout,
		ControlHandlerTimeout:   a.options.ControlHandlerTimeout,
		ObserveStartupStage: func(stageCtx context.Context, stage string, elapsed time.Duration, stageErr error) {
			observe := a.options.RuntimeResourceHooks.ObserveStartupStage
			if observe != nil {
				observe(stageCtx, RuntimeResourceSession, RuntimeStartupStage(stage), elapsed, stageErr)
			}
		},
		SessionMirror: true,
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
		return nil, err
	}

	session := &agentSession{
		agent:                 a,
		id:                    id,
		turn:                  make(chan struct{}, sessionTurnCapacity),
		cwd:                   start.Cwd,
		additionalDirectories: slices.Clone(start.AdditionalDirectories),
		fingerprint:           sessionStartFingerprint(start),
		model:                 defaultModel,
		modelOverrides:        cloneStringMap(modelOverrides),
		mode:                  acpModeForPermission(permissionMode),
		permissionRules:       permissions.Clone(permissionRules),
		materialized:          materialized,
		mcpConfigDir:          mcpConfigDir,
		scratchRootRelease:    scratchRelease,
		mirror:                newSessionMirror(a.log, a.options.SessionStore, processClaudeHome),
		rawMessages:           start.RawMessages,
		mcpRefreshPending:     len(start.McpServers) > 0,
	}
	options.PermissionHandler = session.handlePermission
	options.ElicitationHandler = session.handleElicitation
	options.HookHandler = session.handleHookCallback
	session.clientOptions = options
	session.canRelaunch = true
	session.client = a.newClaudeClient(a.log, options)

	nativeRelease, err = acquireNativeRoot(ctx, a.options.RuntimeResourceHooks, RuntimeResourceSession)
	if err != nil {
		return nil, err
	}

	session.nativeRootRelease = nativeRelease
	cleanupClient = session.client

	startCtx, finishStart := a.observe.StartClaudeProcess(ctx, "start")
	startErr := session.client.Start(startCtx)
	finishStart(startErr)

	if startErr != nil {
		return nil, startErr
	}

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

	// The context window stays unknown (0) until the Claude harness reports one
	// through get_context_usage or a result frame; it is never fabricated from a
	// static model-name catalog.
	session.contextWindowSize = 0

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

	cleanupNeeded = false

	return session, nil
}

func closeSessionStartResources(materialized *materializedSession) {
	if materialized != nil {
		_ = materialized.Close()
	}
}

func (a *Agent) mcpConfigForStart(start sessionStart) (string, error) {
	if err := validateMCPServers(start.McpServers); err != nil {
		return "", err
	}

	mcpConfig, err := mapMCPServersToClaude(start.McpServers)
	if err != nil {
		return "", err
	}

	return mcpConfig, nil
}

func validateMCPServers(servers []acp.McpServer) error {
	seen := make(map[string]struct{}, len(servers))
	for index, server := range servers {
		var name string

		switch {
		case server.Stdio != nil:
			name = server.Stdio.Name
		case server.Http != nil:
			name = server.Http.Name
		case server.Sse != nil:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError:  validationUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Sse.Name,
			})
		case server.Acp != nil:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError:  validationUnsupported,
				jsonFieldField:  fmt.Sprintf("mcpServers[%d]", index),
				jsonFieldServer: server.Acp.Name,
			})
		default:
			return acp.NewInvalidParams(map[string]any{
				jsonFieldError: "no_transport",
				jsonFieldField: fmt.Sprintf("mcpServers[%d]", index),
			})
		}

		if strings.TrimSpace(name) == "" {
			return acp.NewInvalidParams(map[string]any{mcpServerNameField(index): validationRequired})
		}

		if _, exists := seen[name]; exists {
			return acp.NewInvalidParams(map[string]any{mcpServerNameField(index): validationDuplicate})
		}

		seen[name] = struct{}{}
	}

	return nil
}

// mcpServerNameField builds the invalid-params data key for the name of the
// MCP server declaration at the given request index.
func mcpServerNameField(index int) string {
	return fmt.Sprintf("mcpServers[%d].name", index)
}

func (a *Agent) storeHasSession(ctx context.Context, sessionID string, cwd string) bool {
	entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{SessionID: sessionID})

	return err == nil && len(entries) > 0
}

func (a *Agent) nativeSessionFallbackEnabled() bool {
	return a.options.SessionStore == nil
}

func (a *Agent) nativeSessionBlocked(ctx context.Context, sessionID acp.SessionId) (bool, error) {
	if a.isDeleted(sessionID) {
		a.retryDeleteNativeTranscript(ctx, sessionID)

		return true, nil
	}

	if a.nativeSessionFallbackEnabled() {
		return false, nil
	}

	entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{SessionID: string(sessionID)})
	if err != nil {
		return false, err
	}

	if len(entries) > 0 {
		return false, nil
	}

	a.retryDeleteNativeTranscript(ctx, sessionID)

	return true, nil
}

func (a *Agent) isDeleted(sessionID acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.deleted[sessionID]

	return ok
}

func (a *Agent) retryDeletedNativeTranscripts(ctx context.Context) {
	a.mu.Lock()

	ids := make([]acp.SessionId, 0, len(a.deleted))
	for id := range a.deleted {
		ids = append(ids, id)
	}
	a.mu.Unlock()

	for _, id := range ids {
		a.retryDeleteNativeTranscript(ctx, id)
	}
}

func (a *Agent) retryDeleteNativeTranscript(ctx context.Context, sessionID acp.SessionId) {
	if err := deleteNativeTranscript(ctx, a.options.Home, string(sessionID)); err != nil {
		a.log.DebugContext(ctx, "retry delete native Claude transcript failed", slog.String(acpFieldSessionID, string(sessionID)), slog.String(jsonFieldError, err.Error()))
	}
}

func (a *Agent) listStoreSessions(ctx context.Context, params acp.ListSessionsRequest) ([]acp.SessionInfo, error) {
	listCtx, cancel := context.WithTimeout(ctx, a.sessionStoreLoadTimeout())
	defer cancel()

	listCtx, finishList := a.observe.StartSessionStore(listCtx, "list")
	summaries, err := a.sessionStore().ListSessions(listCtx)
	finishList(err)

	if err != nil {
		return nil, fmt.Errorf("list session store: %w", err)
	}

	infos := make([]acp.SessionInfo, 0, len(summaries))
	for _, summary := range summaries {
		if !validUUIDShape(summary.SessionID) {
			continue
		}

		if params.Cwd != nil && strings.TrimSpace(*params.Cwd) != "" && summary.Cwd != "" && summary.Cwd != *params.Cwd {
			continue
		}

		entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{SessionID: summary.SessionID})
		if err != nil {
			return nil, err
		}

		title := firstNonEmptyString(summary.Title, storeSessionTitle(summary.SessionID, entries))
		updatedAt := time.UnixMilli(summary.UpdatedAtUnixMilli).UTC().Format(time.RFC3339)
		cwd := summary.Cwd

		if cwd == "" && params.Cwd != nil {
			cwd = *params.Cwd
		}

		infos = append(infos, acp.SessionInfo{
			SessionId: acp.SessionId(summary.SessionID),
			Cwd:       cwd,
			Title:     &title,
			UpdatedAt: &updatedAt,
			Meta:      cloneAnyMap(summary.Meta),
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

func sessionMatchesListFilters(session *agentSession, params acp.ListSessionsRequest) bool {
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
