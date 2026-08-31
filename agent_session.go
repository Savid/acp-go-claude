package claudeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/observer"
	"github.com/savid/acp-go-claude/internal/permissions"
)

var (
	mapMCPServersToClaude = mapper.MCPServersToClaude
)

// NewSession creates and starts a Claude CLI session.
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (resp acp.NewSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/new")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	if lifecycleErr := rejectLifecycleMeta(params.Meta); lifecycleErr != nil {
		return acp.NewSessionResponse{}, lifecycleErr
	}

	metaOptions, err := claudeOptionsFromMetaWithProviderAuth(params.Meta, a.providerAuth != nil)
	if err != nil {
		return acp.NewSessionResponse{}, err
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

	session, err := a.startAndStoreSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            params.McpServers,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	})
	if err != nil {
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

	if lifecycleErr := rejectLifecycleMeta(params.Meta); lifecycleErr != nil {
		return acp.ResumeSessionResponse{}, lifecycleErr
	}

	metaOptions, err := claudeOptionsFromMetaWithProviderAuth(params.Meta, a.providerAuth != nil)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
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
		if updateErr := session.emitCurrentUsageUpdate(ctx); updateErr != nil {
			return acp.ResumeSessionResponse{}, updateErr
		}

		resp = acp.ResumeSessionResponse{
			Meta:          sessionReuseResponseMeta(session, metaOptions.ProviderAuth),
			ConfigOptions: sessionConfigOptions(session),
		}

		return resp, nil
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.ResumeSessionResponse{}, openErr
	}

	storeEntries, storeErr := a.storedSessionEntries(ctx, params.SessionId)
	if storeErr != nil {
		return acp.ResumeSessionResponse{}, storeErr
	}

	start.StoreEntries = storeEntries

	session, err := a.startAndStoreSession(ctx, params.SessionId, start)
	if err != nil {
		if missingClaudeSessionError(err) {
			return acp.ResumeSessionResponse{}, unknownSessionError()
		}

		return acp.ResumeSessionResponse{}, err
	}

	if err := session.emitCurrentUsageUpdate(ctx); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

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

	if lifecycleErr := rejectLifecycleMeta(params.Meta); lifecycleErr != nil {
		return acp.LoadSessionResponse{}, lifecycleErr
	}

	metaOptions, err := claudeOptionsFromMetaWithProviderAuth(params.Meta, a.providerAuth != nil)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.LoadSessionResponse{}, validationErr
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.LoadSessionResponse{}, openErr
	}

	storeEntries, err := a.storedSessionEntries(ctx, params.SessionId)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}

	start := sessionStart{
		Cwd:                   params.Cwd,
		McpServers:            params.McpServers,
		AdditionalDirectories: additionalDirectories,
		ResumeID:              string(params.SessionId),
		StoreEntries:          storeEntries,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	}

	session := a.activeSessionForStart(params.SessionId, start)
	startedSession := false

	// A live session whose incarnation this adapter had to contain resumes
	// nothing. The transcript this load would replay continues a projection the
	// host has already been told has a hole in it, and the process behind that
	// incarnation is gone or going.
	if session != nil {
		if failureErr := session.autonomousFailureError(); failureErr != nil {
			return acp.LoadSessionResponse{}, failureErr
		}
	}

	if session == nil {
		session, err = a.startAndStoreSession(ctx, params.SessionId, start)
		if err != nil {
			if missingClaudeSessionError(err) {
				return acp.LoadSessionResponse{}, unknownSessionError()
			}

			return acp.LoadSessionResponse{}, err
		}

		startedSession = true
	}

	if replayErr := session.replayTranscriptEntries(ctx, storeEntries); replayErr != nil {
		if startedSession {
			a.removeSession(ctx, params.SessionId, session)
		}

		return acp.LoadSessionResponse{}, replayErr
	}

	if err := session.emitCurrentUsageUpdate(ctx); err != nil {
		return acp.LoadSessionResponse{}, err
	}

	resp = acp.LoadSessionResponse{
		Meta:          sessionLoadResponseMeta(session, metaOptions.ProviderAuth, startedSession),
		ConfigOptions: sessionConfigOptions(session),
	}

	return resp, nil
}

// ListSessions lists active sessions and sessions held by the authoritative store.
func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (resp acp.ListSessionsResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/list")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	if lifecycleErr := rejectLifecycleMeta(params.Meta); lifecycleErr != nil {
		return acp.ListSessionsResponse{}, lifecycleErr
	}

	if validationErr := validateOptionalAbsolutePath("cwd", params.Cwd); validationErr != nil {
		return acp.ListSessionsResponse{}, validationErr
	}

	a.mu.Lock()

	activeSessions := make(map[acp.SessionId]*agentSession, len(a.sessions))
	for id, session := range a.sessions {
		// A tombstoned id is hidden from the moment its tombstone is durable,
		// whatever teardown that delete still owes. A deleted session is
		// wire-indistinguishable from one that never existed.
		if a.isDeletedLocked(id) {
			continue
		}

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

	storeSessions, err := a.listStoreSessions(ctx, params)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}

	sessions := make([]acp.SessionInfo, 0, len(active)+len(storeSessions))
	seen := make(map[acp.SessionId]struct{}, len(active)+len(storeSessions))

	for _, session := range active {
		sessions = append(sessions, session)
		seen[session.SessionId] = struct{}{}
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
//
// Removal follows the completed boundary rather than the request. A close the
// settlement barrier never admitted contained nothing and settled nothing; a
// close that reached the boundary and failed a rung of it still owes that rung.
// Either way the id keeps the session it names, so whatever is still running
// behind it stays addressable and the next close retakes the boundary — dropping
// the id would leave the host holding the only name for work this adapter has
// just failed to finish.
//
// The whole close is session.Close and nothing before it. A native interrupt
// hoisted ahead of it would run rung 5 of the shutdown ladder before rungs 1
// through 4 — the pending input the ladder resolves first, and the provider-auth
// flows it cancels before any teardown, would both be answered by a process that
// was already torn down — and it would tear that process down before the session
// stopped accepting prompts, leaving a window in which a racing prompt relaunches
// exactly what the close just contained. Close already cancels the turn context
// itself, and the turn it wakes runs its own containment on the way out.
func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (resp acp.CloseSessionResponse, err error) {
	ctx, finish := a.observe.StartACP(ctx, params.Meta, "session/close")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	if lifecycleErr := rejectLifecycleMeta(params.Meta); lifecycleErr != nil {
		return acp.CloseSessionResponse{}, lifecycleErr
	}

	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}

	closeErr := session.Close(ctx)
	a.recordContainmentError(closeErr)

	if errors.Is(closeErr, errSessionCloseUnsettled) {
		return acp.CloseSessionResponse{}, sessionCloseUnsettledError(closeErr)
	}

	if closeErr != nil {
		return acp.CloseSessionResponse{}, closeErr
	}

	a.dropSession(ctx, params.SessionId, session)

	return acp.CloseSessionResponse{}, nil
}

// UnstableDeleteSession implements ACP session/delete. The durable tombstone is
// written first and the id is hidden with it, before anything is torn down: a
// crash anywhere below then leaves a session that is already deleted everywhere a
// host can see it, rather than one that is half torn down and still listable.
// Nothing recreates the row afterwards, because a write to a tombstoned key lands
// on nothing — the final mirror commit the teardown below runs included.
//
// Teardown follows, serialized behind the session's own settlement barrier, and
// its errors are reported only from here: the tombstone is already durable and
// the id already hidden, so a partial cleanup is a retry the next list, load,
// resume, or delete takes, not a reason to keep naming a deleted session.
//
// A teardown the barrier never admitted is that same unreached boundary
// session/close reports, and it is answered the same way rather than as an
// internal failure — under its own name, because a refused delete has already
// deleted the session and only owes the teardown.
func (a *Agent) UnstableDeleteSession(
	ctx context.Context,
	params acp.UnstableDeleteSessionRequest,
) (acp.UnstableDeleteSessionResponse, error) {
	if err := rejectLifecycleMeta(params.Meta); err != nil {
		return acp.UnstableDeleteSessionResponse{}, err
	}

	a.mu.Lock()
	session := a.sessions[params.SessionId]
	a.mu.Unlock()

	if err := a.sessionStore().Delete(ctx, SessionKey{SessionID: string(params.SessionId)}); err != nil {
		return acp.UnstableDeleteSessionResponse{}, sessionDeleteTombstoneError(err)
	}

	a.mu.Lock()
	a.deleted[params.SessionId] = struct{}{}
	a.deleteCachedPermissionRulesLocked(params.SessionId)
	a.mu.Unlock()

	var cleanupErr error

	if session != nil {
		// The teardown is the session's own close ladder and nothing hoisted ahead
		// of it, for the reason CloseSession states: rung 5 never runs before rungs
		// 1 through 4, and nothing tears the native process down before the session
		// has stopped admitting the prompt that would relaunch it.
		closeErr := session.Close(ctx)
		if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr)
		}

		// The instance leaves the active map once its close settled. A close the
		// barrier never admitted settled nothing and memoized nothing, so that
		// instance stays for the next delete to retry; the tombstone above already
		// hides it from every request that could address it meanwhile.
		if !errors.Is(closeErr, errSessionCloseUnsettled) {
			a.dropSession(ctx, params.SessionId, session)
		}
	}

	if !a.options.hostAuthoritySet {
		if err := deleteNativeTranscript(ctx, a.options.Home, string(params.SessionId)); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	if cleanupErr != nil {
		a.recordContainmentError(cleanupErr)

		return acp.UnstableDeleteSessionResponse{}, sessionDeleteUnsettledError(cleanupErr)
	}

	return acp.UnstableDeleteSessionResponse{}, nil
}

// session resolves one session-scoped request to its live instance. A tombstoned
// id resolves to nothing whatever the active map still holds: delete hides the id
// the moment its tombstone is durable, and a deleted session is
// wire-indistinguishable from one that never existed. An instance whose teardown
// has not finished stays in the map so a later delete can retry it, but no
// request addresses it again.
func (a *Agent) session(sessionID acp.SessionId) (*agentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.isDeletedLocked(sessionID) {
		return nil, unknownSessionError()
	}

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

// dropSession evicts one session from the active map only while the map still
// holds that exact instance. Removal by id alone would let a closer that raced
// a same-id load, resume or fork evict the live session that replaced it, and
// would decrement the active-session gauge for a removal it never performed.
func (a *Agent) dropSession(ctx context.Context, sessionID acp.SessionId, session *agentSession) {
	a.mu.Lock()
	removed := session != nil && a.sessions[sessionID] == session

	if removed {
		delete(a.sessions, sessionID)
		a.deleteCachedPermissionRulesLocked(sessionID)
	}

	a.mu.Unlock()

	if removed {
		a.observe.AddActiveSession(ctx, -1)
	}
}

// removeSession closes one session and then evicts it. The close runs first
// because it is what proves there is nothing left behind the id: a close its
// settlement barrier never admitted leaves the session in the map, still
// addressable, rather than dropping the handle to work that is still running.
// Close latches the session's terminal state before its own teardown, so every
// door is already refused while the eviction is pending.
func (a *Agent) removeSession(ctx context.Context, sessionID acp.SessionId, session *agentSession) {
	if session != nil {
		if err := session.Close(ctx); err != nil {
			a.recordContainmentError(err)
			a.log.DebugContext(ctx, "close removed Claude session failed", slog.String("class", safeErrorClass(err)))

			if errors.Is(err, errSessionCloseUnsettled) {
				return
			}
		}
	}

	a.dropSession(ctx, sessionID, session)
}

// ensureOpen answers the admission check every entry point runs, and answers it
// already shaped for the wire so the refusals stay apart: a closed agent cannot
// accept the request at all (-32600), a negative concurrency limit is invalid
// params (-32602), and an unusable runtime boundary is the embedding host's
// internal construction failure (-32603).
func (a *Agent) ensureOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errAgentClosed.Error()})
	}

	return a.configurationError()
}

// configurationError reports refused construction options at every entry point.
// Negative public concurrency fields retain their normative invalid-params
// verdict; runtime configuration failures remain internal because the caller's
// method params did not cause them. An in-process host may never call initialize,
// so neither class is allowed to serve work through another door.
func (a *Agent) configurationError() error {
	if a.activeLimitErr != nil {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: a.activeLimitErr.Error()})
	}

	if a.configurationErr != nil {
		return acp.NewInternalError(map[string]any{jsonFieldError: a.configurationErr.Error()})
	}

	return nil
}

// storeStartedSession publishes one freshly started instance under its id, and
// it is the only place an id becomes live. Every reason the id may not be
// published is re-read here, under the lock, after the slow native start: the
// admission checks the entry points ran happen before a process launch that
// takes as long as it takes, so a decision made there is only a guess by the
// time there is something to install.
//
// A tombstone is the reason that guess is load-bearing. Delete writes it and
// hides the id, then tears down whatever the map held; an install that read the
// tombstone before it landed would register a live native session behind it,
// where session, load, resume, and close all answer unknown-session and nothing
// the host can send would ever reach it again. Registration is refused
// instead, the instance nothing will ever name is torn down here, and the caller
// is told what every other door tells it.
func (a *Agent) storeStartedSession(ctx context.Context, session *agentSession) error {
	a.mu.Lock()
	if a.closed {
		a.deleteCachedPermissionRulesLocked(session.id)
		a.mu.Unlock()

		return a.closeRejectedSession(ctx, session, "agent_closed", errAgentClosed)
	}

	if a.isDeletedLocked(session.id) {
		a.deleteCachedPermissionRulesLocked(session.id)
		a.mu.Unlock()

		return a.closeRejectedSession(ctx, session, "session_deleted", unknownSessionError())
	}

	previous := a.sessions[session.id]
	if previous == nil && len(a.sessions) >= a.maxActiveSessions() {
		a.mu.Unlock()

		return a.closeRejectedSession(ctx, session, "active_sessions", backpressureError("active_sessions"))
	}

	if previous != nil {
		a.mu.Unlock()

		// The replaced instance settles before its id names the replacement. An
		// instance the map no longer holds is one nothing can ever close, so a close
		// the settlement barrier did not admit refuses the install rather than
		// orphaning a live incarnation, and the durable commit of the instance on its
		// way out lands before the one taking its place can write the same row.
		if err := a.closeReplacedSession(ctx, session, previous); err != nil {
			return err
		}

		a.mu.Lock()

		if a.closed {
			a.deleteCachedPermissionRulesLocked(session.id)
			a.mu.Unlock()

			// The agent closed while the replaced instance was settling, so this id
			// names nothing: the settled instance leaves the map and the replacement
			// that would have taken its place is torn down.
			a.dropSession(ctx, session.id, previous)

			return a.closeRejectedSession(ctx, session, "agent_closed", errAgentClosed)
		}

		if a.isDeletedLocked(session.id) {
			a.deleteCachedPermissionRulesLocked(session.id)
			a.mu.Unlock()

			// A delete landed while the replaced instance was settling. The tombstone
			// is durable, so the settled instance leaves the map and the replacement
			// is torn down rather than installed behind it.
			a.dropSession(ctx, session.id, previous)

			return a.closeRejectedSession(ctx, session, "session_deleted", unknownSessionError())
		}
	}

	a.sessions[session.id] = session
	a.mu.Unlock()

	// This is the one place an id becomes live, so it is where the provider-auth
	// close mark is cleared: session/close drops an id without tombstoning it and
	// load, resume, and fork all republish a caller-supplied one. The clear runs
	// after the replaced instance's close because that close marks this very id
	// on its way out.
	a.providerAuth.reopenSession(session.id)

	if previous != nil {
		return nil
	}

	a.observe.AddActiveSession(ctx, 1)

	return nil
}

// closeRejectedSession tears down a session the map refused and answers with the
// refusal itself. The session never became addressable, so its close failure is
// recorded and logged rather than reported: the caller asked for admission, and
// admission is what was denied.
//
// That teardown is detached from the caller's cancellation. No id names this
// session, so no later close can reach it — a caller that has already given up
// must not be what decides whether the process it launched is contained. No turn
// can hold a session that was never published, so the barrier is free.
func (a *Agent) closeRejectedSession(
	ctx context.Context,
	session *agentSession,
	reason string,
	refusal error,
) error {
	ctx = context.WithoutCancel(ctx)

	if err := session.Close(ctx); err != nil {
		a.recordContainmentError(err)
		a.log.DebugContext(ctx, "close unadmitted Claude session failed",
			slog.String(jsonFieldReason, reason),
			slog.String("class", safeErrorClass(err)),
		)
	}

	return refusal
}

// closeReplacedSession settles the instance a same-id install is replacing. A
// close its settlement barrier did not admit leaves live native work behind an id
// that is about to name something else, so that install is refused and the
// replacement is torn down instead: the replaced instance keeps the id, stays
// addressable, and can still be closed.
func (a *Agent) closeReplacedSession(ctx context.Context, session *agentSession, previous *agentSession) error {
	closeErr := previous.Close(ctx)
	if closeErr == nil {
		return nil
	}

	a.recordContainmentError(closeErr)
	a.log.WarnContext(ctx, "close replaced Claude session failed", slog.String("class", safeErrorClass(closeErr)))

	if !errors.Is(closeErr, errSessionCloseUnsettled) {
		return nil
	}

	return a.closeRejectedSession(ctx, session, "replaced_session_unsettled", sessionCloseUnsettledError(closeErr))
}

func (a *Agent) startAndStoreSession(
	ctx context.Context,
	id acp.SessionId,
	start sessionStart,
) (*agentSession, error) {
	if err := a.beginSessionConstruction(); err != nil {
		return nil, err
	}
	defer a.endSessionConstruction()

	session, err := a.startSession(ctx, id, start)
	if err != nil {
		return nil, err
	}

	if start.ForkSession {
		session.persistPermissionRules(ctx)
	}

	if err := a.storeStartedSession(ctx, session); err != nil {
		return nil, err
	}

	return session, nil
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

	return caps != nil && caps.Form != nil
}

func (a *Agent) clientSupportsURLElicitation() bool {
	caps := a.clientElicitationCapabilities()

	return caps != nil && caps.Url != nil
}

func (a *Agent) clientSupportsTerminalOutput() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	enabled, _ := a.clientCapabilities.Meta[clientMetaTerminalOutput].(bool)

	return enabled
}

func (a *Agent) activeSessionForStart(id acp.SessionId, start sessionStart) *agentSession {
	fingerprint := sessionStartFingerprint(start)

	a.mu.Lock()
	session := a.sessions[id]
	deleted := a.isDeletedLocked(id)
	a.mu.Unlock()

	// A tombstoned id names nothing whatever the map still holds. Delete leaves an
	// instance behind its tombstone when the teardown it owes has not finished, so
	// reuse reads the tombstone rather than the map and never hands a deleted
	// session back to a host that asked to load or resume it.
	if deleted {
		return nil
	}

	if session == nil || session.fingerprint != fingerprint {
		return nil
	}

	// A session whose close has begun is on its way out of the map, so reusing
	// it would hand the caller a handle onto a native process being torn down.
	if session.isClosing() {
		return nil
	}

	return session
}

func sessionStartFingerprint(start sessionStart) string {
	servers := slices.Clone(start.McpServers)
	slices.SortFunc(servers, func(left, right acp.McpServer) int {
		return strings.Compare(mcpServerName(left), mcpServerName(right))
	})

	metaOptions := start.MetaOptions
	metaOptions.ProviderAuth = nil

	data := struct {
		Cwd                   string           `json:"cwd"`
		AdditionalDirectories []string         `json:"additionalDirectories,omitempty"`
		McpServers            []acp.McpServer  `json:"mcpServers,omitempty"`
		MetaOptions           ClaudeOptions    `json:"metaOptions,omitzero"`
		ProviderAuth          string           `json:"providerAuth,omitempty"`
		RawMessages           rawMessageConfig `json:"rawMessages,omitzero"`
	}{
		Cwd:                   start.Cwd,
		AdditionalDirectories: slices.Clone(start.AdditionalDirectories),
		McpServers:            servers,
		MetaOptions:           metaOptions,
		ProviderAuth:          providerAuthFingerprint(start.MetaOptions.ProviderAuth),
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
// session id cannot be resolved (not active, not in the store, or tombstoned).
// All such cases share one invalid-params shape.
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
	defer func() { a.recordContainmentError(err) }()

	if start.ResumeID != "" && len(start.StoreEntries) == 0 && !start.ActiveSessionResume {
		return nil, unknownSessionError()
	}

	scratchParent, err := a.ensureScratchParent()
	if err != nil {
		return nil, err
	}

	var (
		materialized    *materializedSession
		mcpConfigDir    string
		imageScratchDir string
		cleanupClient   *claude.Client
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

		err = finalizeSessionRuntimeResources(errors.Join(err, closeErr), mcpConfigDir, imageScratchDir, materialized)
	}()

	claudeHome, err := canonicalClaudeHome(a.options.Home)
	if err != nil {
		return nil, err
	}

	imageScratchDir, err = createImageScratchDir(scratchParent)
	if err != nil {
		return nil, err
	}

	imageArtifacts, err := a.loadImageArtifacts(ctx, start.ResumeID)
	if err != nil {
		return nil, err
	}

	if start.ForkSession && string(id) != start.ResumeID {
		if copyErr := a.copyImageArtifacts(ctx, string(id), imageArtifacts); copyErr != nil {
			return nil, copyErr
		}
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

	injection := authInjectionResult{}
	if a.providerAuth != nil {
		injection = a.providerAuth.inject(ctx, env, start.MetaOptions.ProviderAuth)
	}

	env = a.observe.InjectTraceEnv(ctx, env)

	if len(start.StoreEntries) > 0 {
		rehydrated, rehydrateErr := rehydrateTranscriptImageEntries(start.StoreEntries, imageArtifacts)
		if rehydrateErr != nil {
			return nil, rehydrateErr
		}

		materialized, err = a.materializeStoreSessionWithEntries(
			ctx, start.ResumeID, start.Cwd, claudeHome, rehydrated,
		)
	}

	if err != nil {
		return nil, err
	}

	if materialized == nil && a.options.hostAuthoritySet {
		isolatedHome, createErr := materializeMkdirTemp(scratchParent, "acp-go-claude-home-*")
		if createErr != nil {
			return nil, fmt.Errorf("create isolated Claude home: %w", createErr)
		}

		materialized = &materializedSession{configDir: isolatedHome}
		if copyErr := copyClaudeConfigFiles(isolatedHome, claudeHome, a.resumeCredentialOptions()); copyErr != nil {
			return nil, copyErr
		}
	}

	processClaudeHome := claudeHome
	if materialized != nil {
		processClaudeHome = materialized.configDir
	}

	settingsFileArg, err := a.prepareSeededClaudeConfig(claudeHome, processClaudeHome)
	if err != nil {
		return nil, err
	}

	mcpConfigPath, mcpConfigDir, err := writeSessionMCPConfig(scratchParent, mcpConfig)
	if err != nil {
		return nil, err
	}

	if a.options.hostAuthoritySet {
		if materialized == nil {
			return nil, ErrHostAuthorityUnavailable
		}

		if prepareErr := materialized.prepare(ctx, a.options.HostAuthority, processClaudeHome, mcpConfigDir); prepareErr != nil {
			return nil, prepareErr
		}
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
		OrdinaryEnvironment:     a.ordinaryEnvironment(),
		Authority:               a.claudeAuthority(),
		TreePrepared:            a.options.hostAuthoritySet,
		ExtraPathDirs:           slices.Clone(start.MetaOptions.ExtraPathDirs),
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
		imageScratchDir:       imageScratchDir,
		imageArtifacts:        imageArtifacts,
		toolContent:           make(map[acp.ToolCallId][]acp.ToolCallContent),
		emittedAgentImages:    make(map[string]struct{}),
		rawMessages:           start.RawMessages,
		mcpRefreshPending:     len(start.McpServers) > 0,
		providerAuthInjection: injection.outcome,
		providerAuthResident:  injection.resident,
	}
	session.mirror = newSessionMirror(a.log, a.sessionStore(), processClaudeHome, session)
	options.PermissionHandler = session.handlePermission
	options.ElicitationHandler = session.handleElicitation
	options.HookHandler = session.handleHookCallback
	session.clientOptions = options
	session.canRelaunch = true

	session.client = a.newClaudeClient(a.log, options)
	if _, local := a.connection().(*localAgentConnection); local {
		if installErr := session.installEstablishmentGate(session.client); installErr != nil {
			return nil, installErr
		}

		defer func() {
			if err != nil {
				session.settleEstablishment(err)
			}
		}()
	}

	session.client.SetControlHandlerAdmission(session.admitControlCallback)

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
		a.log.DebugContext(ctx, "get Claude settings failed", slog.String("stage", "settings_read"))

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

func (a *Agent) storedSessionEntries(ctx context.Context, sessionID acp.SessionId) ([]SessionStoreEntry, error) {
	if a.isDeleted(sessionID) {
		a.retryDeleteResidualNativeTranscript(ctx, sessionID)

		return nil, unknownSessionError()
	}

	entries, err := a.loadStoreEntries(ctx, a.sessionStore(), SessionKey{SessionID: string(sessionID)})
	if err != nil {
		return nil, err
	}

	if len(entries) > 0 {
		return entries, nil
	}

	a.retryDeleteResidualNativeTranscript(ctx, sessionID)

	return nil, unknownSessionError()
}

// retryDeleteResidualNativeTranscript removes only inactive native cache state.
// An active binding remains usable through its bound route even when a
// store-authoritative load or changed-fingerprint resume fails unknown.
func (a *Agent) retryDeleteResidualNativeTranscript(ctx context.Context, sessionID acp.SessionId) {
	a.mu.Lock()
	active := a.sessions[sessionID] != nil
	a.mu.Unlock()

	if active {
		return
	}

	a.retryDeleteNativeTranscript(ctx, sessionID)
}

func (a *Agent) isDeleted(sessionID acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.isDeletedLocked(sessionID)
}

// isDeletedLocked reports the durable tombstone every path that hands out, or
// installs, an instance for this id has to consult. Delete writes the tombstone
// and hides the id in one critical section, so reading it under the same lock is
// what makes "this id names nothing" a fact rather than a stale observation.
func (a *Agent) isDeletedLocked(sessionID acp.SessionId) bool {
	_, ok := a.deleted[sessionID]

	return ok
}

func (a *Agent) retryDeleteNativeTranscript(ctx context.Context, sessionID acp.SessionId) {
	if a.options.hostAuthoritySet {
		return
	}

	if err := deleteNativeTranscript(ctx, a.options.Home, string(sessionID)); err != nil {
		a.log.DebugContext(ctx, "retry delete native Claude transcript failed",
			slog.String(acpFieldSessionID, string(sessionID)), slog.String("class", safeErrorClass(err)))
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
	if params.Cwd != nil && *params.Cwd != "" && *params.Cwd != session.cwd {
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
