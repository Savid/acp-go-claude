package claudeacp

import (
	"context"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

func (s *session) lifecycleNegotiated() lifecycle.Negotiated { return s.agent.lifecycleNegotiated() }

func (s *session) openStream(ctx context.Context) error {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	return s.lc.Open(ctx, fmt.Sprintf("%s:%d", s.id, s.agent.nextIncarnation()), s.lifecycleNegotiated(), s.deliverLifecycle)
}

func (s *session) acceptTurn(ctx context.Context, t *turn) {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	if t.accepted {
		return
	}

	t.accepted = true
	if err := s.lc.Accept(ctx, &t.Cycle, t.submission); err != nil {
		t.keepFirstFailure(err)
	}
}

// recordFailure keeps the first delivery or control failure of one cycle under
// the lock acceptTurn writes it with.
func (s *session) recordFailure(c *cycle, err error) {
	if err == nil {
		return
	}

	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	c.keepFirstFailure(err)
}

// keepFirstFailure records err as the cycle's failure unless one is already
// recorded. Its caller holds the session lcMu.
func (c *cycle) keepFirstFailure(err error) {
	if c.failure == nil {
		c.failure = err
	}
}

// cycleFailure reads the recorded failure under the same lock.
func (s *session) cycleFailure(c *cycle) error {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	return c.failure
}

func (s *session) deliverLifecycle(ctx context.Context, envelope map[string]any) error {
	if conn := s.agent.connection(); conn != nil {
		return conn.SessionUpdate(ctx, acp.SessionNotification{Meta: map[string]any{wire.LifecycleKey: envelope}, SessionId: s.id, Update: acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}}})
	}

	return nil
}
