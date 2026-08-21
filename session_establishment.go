package claudeacp

import (
	"context"
	"errors"
	"sync"

	"github.com/savid/acp-go-claude/internal/claude"
)

type sessionProducerGate struct {
	mu      sync.Mutex
	closing bool
	active  int
	done    chan struct{}
}

func (g *sessionProducerGate) begin() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closing {
		return func() {}, false
	}

	if g.active == 0 {
		g.done = make(chan struct{})
	}

	g.active++

	var once sync.Once

	return func() {
		once.Do(func() {
			g.mu.Lock()

			g.active--
			if g.active == 0 {
				close(g.done)
			}
			g.mu.Unlock()
		})
	}, true
}

func (g *sessionProducerGate) closeAndWait(ctx context.Context) error {
	g.mu.Lock()

	g.closing = true
	if g.active == 0 {
		g.mu.Unlock()

		return nil
	}

	done := g.done
	g.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *sessionProducerGate) seal() {
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
}

type sessionEstablishment struct {
	client *claude.Client
	route  string
	done   chan struct{}
	once   sync.Once

	mu  sync.Mutex
	err error
}

func (e *sessionEstablishment) settle(err error) {
	if e == nil {
		return
	}

	e.once.Do(func() {
		e.mu.Lock()
		e.err = err
		e.mu.Unlock()
		close(e.done)
	})
}

func (e *sessionEstablishment) succeeded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.err == nil
}

func (s *agentSession) installEstablishmentGate(client *claude.Client) error {
	if client == nil {
		return errors.New("session establishment requires a native client")
	}

	route, err := newUUID()
	if err != nil {
		return err
	}

	gate := &sessionEstablishment{client: client, route: route, done: make(chan struct{})}

	s.establishmentMu.Lock()
	s.establishment = gate
	s.establishmentMu.Unlock()

	client.AdoptControlRoute(route)

	return nil
}

func (s *agentSession) establishmentRoute(client *claude.Client) string {
	s.establishmentMu.Lock()
	defer s.establishmentMu.Unlock()

	if s.establishment == nil || s.establishment.client != client {
		return ""
	}

	return s.establishment.route
}

func (s *agentSession) awaitEstablishmentRoute(ctx context.Context, route string) bool {
	s.establishmentMu.Lock()
	gate := s.establishment
	s.establishmentMu.Unlock()

	if gate == nil || gate.route != route {
		return true
	}

	select {
	case <-gate.done:
		return gate.succeeded()
	case <-ctx.Done():
		return false
	}
}

func (s *agentSession) settleEstablishment(err error) {
	s.establishmentMu.Lock()
	gate := s.establishment
	s.establishmentMu.Unlock()
	gate.settle(err)
}

func (s *agentSession) completeEstablishment(ctx context.Context) error {
	if err := s.serveNativePump(ctx, s.currentClient()); err != nil {
		s.settleEstablishment(err)

		return err
	}

	if err := s.emitAvailableCommandsUpdate(ctx, true); err != nil {
		s.settleEstablishment(err)

		return err
	}

	s.settleEstablishment(nil)

	return nil
}
