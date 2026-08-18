package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

var stableMCPServers = mapper.StableMCPServers

// rateLimitsProbeTimeout bounds one `claude /usage` probe run.
const rateLimitsProbeTimeout = 60 * time.Second

// rateLimitsAPITimeout bounds one direct Anthropic API usage probe.
const rateLimitsAPITimeout = 30 * time.Second

// rateLimitsAPITTL bounds how long a direct API result is reused. The API
// fallback can cost a billable inference request, so a chatty poller must not
// turn `_claude/rateLimits` into a stream of them.
const rateLimitsAPITTL = 60 * time.Second

// rateLimitsCacheEntry memoizes the most recent direct API result.
type rateLimitsCacheEntry struct {
	limits  claude.RateLimits
	fetched time.Time
}

// RateLimitsResponse is the `_claude/rateLimits` response payload. Windows is
// empty when the harness reports no subscription usage; values are only ever
// harness-reported.
type RateLimitsResponse struct {
	Windows  []RateLimitWindow `json:"windows"`
	PlanType string            `json:"planType,omitempty"`
}

// RateLimitWindow is one harness-reported subscription usage window.
type RateLimitWindow struct {
	// ID is the vendor-native window id, e.g. "session" or "week-all-models".
	ID string `json:"id"`
	// UsedPercent is the harness-reported percentage of the window consumed.
	UsedPercent float64 `json:"usedPercent"`
	// ResetsAt is the RFC3339 reset time, omitted when not reported.
	ResetsAt string `json:"resetsAt,omitempty"`
}

func (a *Agent) handleRateLimits(ctx context.Context, raw json.RawMessage) (_ RateLimitsResponse, returnErr error) {
	if err := validateEmptyParams(raw); err != nil {
		return RateLimitsResponse{}, err
	}

	if err := a.beginSessionConstruction(); err != nil {
		return RateLimitsResponse{}, err
	}
	defer func() {
		a.recordContainmentError(returnErr)
		a.endSessionConstruction()
	}()

	// Home resolution shares the probes' degrade contract: a misconfigured
	// Claude home leaves nothing to probe, so the request answers with empty
	// windows — the same answer a failed probe produces — instead of failing.
	claudeHome, err := canonicalClaudeHome(a.options.Home)
	if err != nil {
		a.log.DebugContext(ctx, "resolve Claude home failed", slog.String(jsonFieldError, err.Error()))

		return RateLimitsResponse{Windows: make([]RateLimitWindow, 0)}, nil
	}

	claudeOptions := claude.Options{
		CLIPath:             a.options.ExecutablePath,
		ClaudeHome:          claudeHome,
		Env:                 a.options.Env,
		ProcessIsolation:    a.claudeIsolation(),
		OrdinaryEnvironment: a.ordinaryEnvironment(),
		DarwinBestEffort:    a.containmentMode == RuntimeContainmentBestEffort,
		AcquireUsageDiscovery: func(discoveryCtx context.Context) (func(), error) {
			return acquireNativeRoot(discoveryCtx, a.options.RuntimeResourceHooks, RuntimeResourceDiscovery)
		},
		PrepareUsageGeneration: func(generationCtx context.Context) (*claude.DarwinGeneration, error) {
			return a.prepareDiscoveryGeneration(generationCtx)
		},
	}
	processSnapshotSource := a.descendantProcesses.newSource()
	claudeOptions.ObserveProcessInventory = processSnapshotSource.started
	claudeOptions.ObserveBoundaryComplete = processSnapshotSource.completed

	probeCtx, cancel := context.WithTimeout(ctx, rateLimitsProbeTimeout)
	defer cancel()

	// A failed probe degrades to empty windows rather than failing the
	// request: reporting no windows is the same answer the harness gives, and
	// a broken probe must not break the extension.
	limits, err := a.queryRateLimits(probeCtx, claudeOptions)
	if err != nil {
		if errors.Is(err, ErrProcessContainmentIncomplete) {
			return RateLimitsResponse{}, err
		}

		a.log.DebugContext(ctx, "claude usage probe failed", slog.String(jsonFieldError, err.Error()))

		limits = claude.RateLimits{}
	}

	// The harness only prints windows for a logged-in, profile-scoped Claude
	// home. Token-authenticated homes report nothing, so read the account's
	// usage from the API instead when the adapter is allowed to.
	if len(limits.Windows) == 0 && a.options.DirectAPI {
		limits = a.rateLimitsFromAPI(ctx, claudeOptions)
	}

	windows := make([]RateLimitWindow, 0, len(limits.Windows))
	for _, window := range limits.Windows {
		resetsAt := ""
		if !window.ResetsAt.IsZero() {
			resetsAt = window.ResetsAt.Format(time.RFC3339)
		}

		windows = append(windows, RateLimitWindow{
			ID:          window.ID,
			UsedPercent: window.UsedPercent,
			ResetsAt:    resetsAt,
		})
	}

	return RateLimitsResponse{Windows: windows}, nil
}

// rateLimitsFromAPI reads usage windows straight from the Anthropic API,
// memoizing the result for rateLimitsAPITTL. A failed probe degrades to empty
// windows rather than failing the request: reporting no windows is the same
// answer the harness gives, and a misconfigured gateway must not break the
// extension.
func (a *Agent) rateLimitsFromAPI(ctx context.Context, options claude.Options) claude.RateLimits {
	a.rateLimitsCacheMu.Lock()
	defer a.rateLimitsCacheMu.Unlock()

	if !a.rateLimitsCache.fetched.IsZero() && time.Since(a.rateLimitsCache.fetched) < rateLimitsAPITTL {
		return a.rateLimitsCache.limits
	}

	probeCtx, cancel := context.WithTimeout(ctx, rateLimitsAPITimeout)
	defer cancel()

	limits, err := a.queryRateLimitsAPI(probeCtx, claude.RateLimitsProbe{
		Options:   options,
		UserAgent: a.options.AgentName + "/" + a.options.AgentVersion,
	})
	if err != nil {
		a.log.DebugContext(ctx, "direct rate-limits API probe failed", slog.String(jsonFieldError, err.Error()))

		return claude.RateLimits{}
	}

	a.rateLimitsCache = rateLimitsCacheEntry{limits: limits, fetched: time.Now()}

	return limits
}

// validateEmptyParams accepts absent, null, or empty-object params and rejects
// everything else by the offending member's own name. Sorting makes the answer
// deterministic when a caller sends several: one request always names the same
// field back.
func validateEmptyParams(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return unsupportedField(jsonFieldParams)
	}

	if keys := slices.Sorted(maps.Keys(params)); len(keys) > 0 {
		return unsupportedField(keys[0])
	}

	return nil
}

// Logout clears auth state owned by this adapter.
func (a *Agent) Logout(_ context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	if err := rejectLifecycleMeta(params.Meta); err != nil {
		return acp.LogoutResponse{}, err
	}

	return acp.LogoutResponse{}, nil
}

func (a *Agent) handleForkSession(
	ctx context.Context,
	raw json.RawMessage,
) (acp.UnstableForkSessionResponse, error) {
	var params acp.UnstableForkSessionRequest
	if err := json.Unmarshal(raw, &params); err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := params.Validate(); err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	metaOptions, err := claudeOptionsFromMetaWithProviderAuth(params.Meta, a.providerAuth != nil)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.UnstableForkSessionResponse{}, validationErr
	}

	mcpServers, err := stableMCPServers(params.McpServers)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	sessionID, err := newUUID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	permissionRules, err := a.permissionRulesForSession(ctx, params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	var storeEntries []SessionStoreEntry

	a.mu.Lock()
	parentActive := a.sessions[params.SessionId] != nil
	a.mu.Unlock()

	if !parentActive {
		storeEntries, err = a.storedSessionEntries(ctx, params.SessionId)
		if err != nil {
			return acp.UnstableForkSessionResponse{}, err
		}
	}

	session, err := a.startAndStoreSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            mcpServers,
		ResumeID:              string(params.SessionId),
		StoreEntries:          storeEntries,
		ActiveSessionResume:   parentActive,
		ForkSession:           true,
		PermissionRules:       permissionRules,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	}, func(session *agentSession) {
		session.persistPermissionRules(ctx)
	})
	if err != nil {
		// Forking an unknown or deleted parent returns the same uniform
		// unknown-session error as every other session-scoped request method,
		// matching resume and load.
		if missingClaudeSessionError(err) {
			return acp.UnstableForkSessionResponse{}, unknownSessionError()
		}

		return acp.UnstableForkSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	return acp.UnstableForkSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionUnstableConfigOptions(session),
	}, nil
}
