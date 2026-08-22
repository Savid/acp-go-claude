package claudeacp

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	defaultMaxActiveSessions = 32
	defaultMaxClientCalls    = 16

	// sessionTurnCapacity fixes per-session prompt admission to a single
	// in-flight turn. Claude turn state (cancel handle, turn id) is
	// single-valued, so a second prompt to a busy session is refused with
	// session_prompt backpressure; the capacity is not configurable.
	sessionTurnCapacity = 1
)

type clientCallPermitContextKey struct{}

type clientCallPermit struct{ agent *Agent }

func validateConcurrencyLimits(limits ConcurrencyLimits) error {
	if limits.MaxActiveSessions < 0 {
		return fmt.Errorf("max active sessions must be non-negative")
	}

	if limits.MaxConcurrentClientCalls < 0 {
		return fmt.Errorf("max concurrent client calls must be non-negative")
	}

	return nil
}

// turnTimeout is the configured per-turn deadline. Zero means no deadline.
func (a *Agent) turnTimeout() time.Duration {
	return a.options.TurnTimeout
}

func (a *Agent) maxActiveSessions() int {
	if a.options.ConcurrencyLimits.MaxActiveSessions > 0 {
		return a.options.ConcurrencyLimits.MaxActiveSessions
	}

	return defaultMaxActiveSessions
}

func (a *Agent) maxConcurrentClientCalls() int {
	if a.options.ConcurrencyLimits.MaxConcurrentClientCalls > 0 {
		return a.options.ConcurrencyLimits.MaxConcurrentClientCalls
	}

	return defaultMaxClientCalls
}

func (a *Agent) acquireClientCall(ctx context.Context) (func(), error) {
	if permit, _ := ctx.Value(clientCallPermitContextKey{}).(*clientCallPermit); permit != nil && permit.agent == a {
		return func() {}, nil
	}

	a.mu.Lock()
	if a.clientCalls == nil {
		a.clientCalls = make(chan struct{}, a.maxConcurrentClientCalls())
	}

	calls := a.clientCalls
	a.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, ctx.Err()
	}

	select {
	case calls <- struct{}{}:
		return func() { <-calls }, nil
	default:
		return nil, backpressureError("client_calls")
	}
}

// acquireCallbackClientCall admits one native callback into the outbound-call
// arbiter and returns the causal context its nested host calls share. The one
// admitted callback owns the arbiter while its nested host request completes.
func (a *Agent) acquireCallbackClientCall(ctx context.Context) (context.Context, func(), error) {
	release, err := a.acquireClientCall(ctx)
	if err != nil {
		return ctx, func() {}, err
	}

	return context.WithValue(ctx, clientCallPermitContextKey{}, &clientCallPermit{agent: a}), release, nil
}

func backpressureError(limit string) *acp.RequestError {
	return acp.NewInvalidRequest(map[string]any{
		jsonFieldError: "backpressure",
		jsonFieldLimit: limit,
	})
}
