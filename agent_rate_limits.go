package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	rateLimitsReadTimeout = 30 * time.Second
	rateLimitsReadFailed  = "read_failed"
)

type rateLimitsTarget struct {
	providerID string
	session    *agentSession
	client     *claude.Client
	env        map[string]string
	epoch      uint64
}

func (a *Agent) handleRateLimits(ctx context.Context, raw json.RawMessage) (_ RateLimitsResponse, returnErr error) {
	request, err := decodeRateLimitsRequest(raw)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return RateLimitsResponse{}, openErr
	}

	if contextErr := ctx.Err(); contextErr != nil {
		return RateLimitsResponse{}, contextErr
	}

	defer func() { a.recordContainmentError(returnErr) }()

	target, err := a.rateLimitsTarget(request)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	if target.providerID != authProviderID {
		return rateLimitsUnsupported(target.providerID), nil
	}

	if target.session == nil {
		return rateLimitsUnavailable(target.providerID, "session_required"), nil
	}

	readCtx, cancel := context.WithTimeout(ctx, rateLimitsReadTimeout)
	defer cancel()

	source, readErr := a.queryRateLimits(readCtx, target.client)
	if errors.Is(readErr, ErrContainmentIncomplete) || errors.Is(readErr, ErrHostAuthorityUnavailable) {
		return RateLimitsResponse{}, errors.Join(readErr, ctx.Err())
	}

	if contextErr := ctx.Err(); contextErr != nil {
		return RateLimitsResponse{}, contextErr
	}

	if readCtx.Err() != nil {
		readErr = readCtx.Err()
	}

	current, err := a.rateLimitsTargetCurrent(target)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	if !current {
		return rateLimitsUnavailable(target.providerID, "not_observed"), nil
	}

	if readErr != nil {
		if errors.Is(readErr, claude.ErrRateLimitsDisabled) {
			return rateLimitsUnavailable(target.providerID, "disabled"), nil
		}

		if errors.Is(readErr, claude.ErrRateLimitsNotAuthenticated) {
			return rateLimitsUnavailable(target.providerID, "not_authenticated"), nil
		}

		return rateLimitsUnavailable(target.providerID, rateLimitsReadFailed), nil
	}

	return normalizeRateLimits(target.providerID, source), nil
}

func (a *Agent) rateLimitsTarget(request RateLimitsRequest) (rateLimitsTarget, error) {
	target := rateLimitsTarget{providerID: request.ProviderID}

	a.mu.Lock()
	target.epoch = a.rateLimitsEpoch
	a.mu.Unlock()

	if request.SessionID != "" {
		session, err := a.session(request.SessionID)
		if err != nil {
			return rateLimitsTarget{}, err
		}

		session.mu.Lock()
		err = rateLimitsSessionError(session)
		target.session, target.client = session, session.client
		target.env = maps.Clone(session.clientOptions.Env)
		session.mu.Unlock()

		if err != nil {
			return rateLimitsTarget{}, err
		}
	}

	if target.providerID == "" {
		target.providerID = authProviderID
	}

	return target, nil
}

// rateLimitsSessionError is called with the session lock held.
func rateLimitsSessionError(session *agentSession) error {
	if session.poisonCause != "" {
		return poisonedSessionError(session.poisonCause)
	}

	if session.closing {
		return closedSessionError()
	}

	return session.autonomousErr
}

func (a *Agent) rateLimitsTargetCurrent(target rateLimitsTarget) (bool, error) {
	if err := a.ensureOpen(); err != nil {
		return false, err
	}

	a.mu.Lock()
	current := target.epoch == a.rateLimitsEpoch
	a.mu.Unlock()

	session, err := a.session(target.session.id)
	if err != nil {
		return false, err
	}

	if session != target.session {
		return false, nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if err := rateLimitsSessionError(session); err != nil {
		return false, err
	}

	return current && session.client == target.client &&
		maps.Equal(session.clientOptions.Env, target.env), nil
}

func normalizeRateLimits(providerID string, source claude.RateLimits) RateLimitsResponse {
	if source.Unsupported {
		return rateLimitsUnsupported(providerID)
	}

	response := rateLimitsUnavailable(providerID, "not_observed")
	if source.ObservedAt.IsZero() || time.Since(source.ObservedAt) >= time.Minute {
		return response
	}

	for _, nativePool := range source.Pools {
		pool := RateLimitPool{ID: nativePool.ID, Label: nativePool.Label, Windows: []RateLimitWindow{}}
		if strings.TrimSpace(source.PlanType) != "" {
			pool.PlanType = source.PlanType
		}

		for _, nativeWindow := range nativePool.Windows {
			if !nativeWindow.ResetsAt.IsZero() && !nativeWindow.ResetsAt.After(time.Now()) {
				continue
			}

			window := RateLimitWindow{
				ID: nativeWindow.ID, UsedPercent: &nativeWindow.UsedPercent,
				ObservedAt: source.ObservedAt.UTC().Format(time.RFC3339Nano),
			}
			if !nativeWindow.ResetsAt.IsZero() {
				window.ResetsAt = nativeWindow.ResetsAt.UTC().Format(time.RFC3339Nano)
			}

			if nativeWindow.DurationSeconds > 0 {
				window.DurationSeconds = &nativeWindow.DurationSeconds
			}

			pool.Windows = append(pool.Windows, window)
		}

		if len(pool.Windows) > 0 {
			response.Pools = append(response.Pools, pool)
		}
	}

	if len(response.Pools) > 0 {
		response.Availability, response.Reason = "available", ""
	}

	return response
}

func (a *Agent) invalidateRateLimits() {
	a.mu.Lock()
	a.rateLimitsEpoch++
	a.mu.Unlock()
}
