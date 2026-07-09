package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

func (a *Agent) handleRateLimits(ctx context.Context, raw json.RawMessage) (RateLimitsResponse, error) {
	if err := validateEmptyParams(raw); err != nil {
		return RateLimitsResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := a.ensureOpen(); err != nil {
		return RateLimitsResponse{}, err
	}

	claudeHome, err := canonicalClaudeHome(a.options.Home)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	claudeOptions := claude.Options{
		CLIPath:    a.options.ExecutablePath,
		ClaudeHome: claudeHome,
		Env:        a.options.Env,
	}

	probeCtx, cancel := context.WithTimeout(ctx, rateLimitsProbeTimeout)
	defer cancel()

	limits, err := a.queryRateLimits(probeCtx, claudeOptions)
	if err != nil {
		return RateLimitsResponse{}, err
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
// everything else.
func validateEmptyParams(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var params struct{}

	return decoder.Decode(&params)
}

// Logout clears auth state owned by this adapter.
func (a *Agent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
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

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, lifecycleMetaError(err)
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

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.UnstableForkSessionResponse{}, openErr
	}

	session, err := a.startSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            mcpServers,
		ResumeID:              string(params.SessionId),
		ForkSession:           true,
		PermissionRules:       permissionRules,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
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

	session.persistPermissionRules(ctx)

	if err := a.storeStartedSession(ctx, session); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	return acp.UnstableForkSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionUnstableConfigOptions(session),
	}, nil
}
