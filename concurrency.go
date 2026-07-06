package claudeacp

import (
	"context"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

const (
	defaultMaxActiveSessions        = 32
	defaultMaxConcurrentPrompts     = 1
	defaultMaxConcurrentClientCalls = 16
)

func validateConcurrencyLimits(limits ConcurrencyLimits) error {
	if limits.MaxActiveSessions < 0 {
		return fmt.Errorf("max active sessions must be non-negative")
	}

	if limits.MaxConcurrentPrompts < 0 {
		return fmt.Errorf("max concurrent prompts must be non-negative")
	}

	if limits.MaxConcurrentClientCalls < 0 {
		return fmt.Errorf("max concurrent client calls must be non-negative")
	}

	return nil
}

func (a *Agent) maxActiveSessions() int {
	if a.options.ConcurrencyLimits.MaxActiveSessions > 0 {
		return a.options.ConcurrencyLimits.MaxActiveSessions
	}

	return defaultMaxActiveSessions
}

func (a *Agent) maxConcurrentPrompts() int {
	if a.options.ConcurrencyLimits.MaxConcurrentPrompts > 0 {
		return a.options.ConcurrencyLimits.MaxConcurrentPrompts
	}

	return defaultMaxConcurrentPrompts
}

func (a *Agent) maxConcurrentClientCalls() int {
	if a.options.ConcurrencyLimits.MaxConcurrentClientCalls > 0 {
		return a.options.ConcurrencyLimits.MaxConcurrentClientCalls
	}

	return defaultMaxConcurrentClientCalls
}

func (a *Agent) acquireClientCall(ctx context.Context) (func(), error) {
	a.mu.Lock()
	if a.clientCalls == nil {
		a.clientCalls = make(chan struct{}, a.maxConcurrentClientCalls())
	}

	calls := a.clientCalls
	a.mu.Unlock()

	select {
	case calls <- struct{}{}:
		return func() { <-calls }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, backpressureError("client_calls")
	}
}

func (s *agentSession) maxConcurrentPrompts() int {
	if s.agent != nil && s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts > 0 {
		return s.agent.options.ConcurrencyLimits.MaxConcurrentPrompts
	}

	return defaultMaxConcurrentPrompts
}

func backpressureError(limit string) *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{
		jsonFieldError: "backpressure",
		"limit":        limit,
	})
}
