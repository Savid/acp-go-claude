package claudeacp

import (
	"context"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// The ACP SDK dispatches every inbound request in its own goroutine and cancels
// that request's context when the handler returns; only notifications are
// serialized. Two legs addressing one flow therefore run at the same time, and
// every read state → native call → write state sequence on this surface is a
// check-then-set whose window is the whole native call. The primitives here
// close those windows. They are not race fixes: every field access below is
// already mutex-guarded, so the detector reports nothing while two legs both
// pass one check and both act on it.

// authGate is one single-holder gate together with the number of legs that hold
// it or are waiting for it. The count is the gate's whole lifetime: the keys are
// (session, provider) pairs and native homes, unbounded over the life of an
// agent that outlives its sessions, so a map that only grows is a leak.
type authGate struct {
	ch      chan struct{}
	waiters int
}

// authAcquireGate takes the gate named by key, creating it on first use, and
// returns the release its holder defers. It reports false only when ctx ended
// first, which is the one way a leg leaves without the gate and so without the
// right to mutate what the gate names.
//
// authDropGate is the only thing that may remove an entry, and only once the
// last leg has left. Deleting a gate any other way — sweeping a closed session's
// entries, for instance — replaces a gate a leg still holds with a fresh one the
// next leg walks straight through, so the serialization silently stops happening
// while every map operation still looks correct.
func authAcquireGate[K comparable](ctx context.Context, mu *sync.Mutex, gates map[K]*authGate, key K) (func(), bool) {
	mu.Lock()

	gate, ok := gates[key]
	if !ok {
		gate = &authGate{ch: make(chan struct{}, 1)}
		gates[key] = gate
	}

	gate.waiters++
	mu.Unlock()

	select {
	case gate.ch <- struct{}{}:
		return func() {
			<-gate.ch

			authDropGate(mu, gates, key, gate)
		}, true
	case <-ctx.Done():
		authDropGate(mu, gates, key, gate)

		return nil, false
	}
}

// authDropGate accounts for one leg leaving and removes the gate once it was the
// last. A gate nobody holds or wants orders nothing, so the next leg to name
// that key can be handed a new one.
func authDropGate[K comparable](mu *sync.Mutex, gates map[K]*authGate, key K, gate *authGate) {
	mu.Lock()
	defer mu.Unlock()

	gate.waiters--
	if gate.waiters == 0 {
		delete(gates, key)
	}
}

// admitKey gates every authorize for one (session, provider) pair against every
// other. It is held from before the retired check to after the mint has settled,
// which is what makes the sequence retired → replay → supersede → record →
// publish → mint atomic against a second authorize, and so what makes the
// idempotency key mean anything at all. Without it two identical requests never
// see each other — neither has published when the other looks — and both start a
// login child, after which only one is reachable and the other is a live
// `claude auth login` nothing can ever fence.
func (p *providerAuth) admitKey(ctx context.Context, key authFlowKey) (func(), bool) {
	return authAcquireGate(ctx, &p.mu, p.admissions, key)
}

// admitSlot gates every mutation of the native home's credential slot against
// every other. The key is the home rather than the provider because that is what
// this surface's removal clears: `claude auth logout` plus the keystore wipe act
// on the config dir as a whole, so per-provider keying would name something no
// leg here actually reserves. Held across a completion's lineage check,
// confirmation, and the reading they rest on, and across a disconnect's whole
// bump → clear → prove-absent → record sequence, the two can never interleave
// into a binding generation that goes backwards.
func (p *providerAuth) admitSlot(ctx context.Context) (func(), bool) {
	return authAcquireGate(ctx, &p.mu, p.slots, p.home.path)
}

// sessionClosed reports whether the session has already had its flows swept.
// The mark and the sweep set are taken in one critical section, so an id that
// reads closed here is one whose cleanup has already been decided. Refusing a
// leg on it is the cheap path; publication is the authoritative one.
func (p *providerAuth) sessionClosed(sessionID acp.SessionId) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, closed := p.closedSessions[sessionID]

	return closed
}

// publishFlow makes the flow addressable and is the authoritative session
// admission check: an authorize that passed its session lookup a moment before
// close took its cleanup set is refused here, because a record published after
// that set was taken is one close can no longer see, and the login child it
// would go on to start writes into the config dir with nothing left to fence it.
//
// Close deliberately does not wait for the legs already in flight. Draining
// would block session/close for the length of an unbounded native call, and
// refusing publication holds the same invariant without it: no flow escapes
// close's cleanup set.
func (p *providerAuth) publishFlow(key authFlowKey, flow *authFlow) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, closed := p.closedSessions[flow.sessionID]; closed {
		return false
	}

	p.flows[key] = flow
	p.byID[flow.id] = flow

	return true
}

// reopenSession clears the mark a session id carries once that id is live again.
// session/close drops the id from the live session map without tombstoning it —
// only session/delete tombstones — so a later load, resume, or fork rebuilds a
// session under exactly that id. A mark left behind would refuse every
// provider-auth leg for it for the rest of the agent's life, on the strength of
// a lifetime that already ended.
func (p *providerAuth) reopenSession(sessionID acp.SessionId) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.closedSessions, sessionID)
}

// claimFlow admits the one leg that may drive this flow's login child and holds
// the claim for the whole attempt. The terminal read and the claim are one
// critical section because a native call sits between them: two callbacks that
// both pass a check neither has set yet both write to and then close one child's
// stdin, losing a valid paste or reporting a refusal of a login that in fact
// succeeded. The status probe takes the same claim inline with its poll floor
// and skips a busy flow rather than queueing behind it, since whoever holds the
// claim is already driving the same completion and a consumer's poll cadence
// must never end up behind a native call.
func (p *providerAuth) claimFlow(flow *authFlow) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if authTerminal(flow.state) || flow.claimed {
		return authFailed(authCauseFlowState, flow.providerID, flow.method.ID, flow.id)
	}

	flow.claimed = true

	return nil
}

// releaseFlow drops the claim. A flow that terminalized under the claim refuses
// a later one anyway, so releasing after success costs nothing and keeps every
// claimant's shape the same.
func (p *providerAuth) releaseFlow(flow *authFlow) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow.claimed = false
}

// requestRetired reports whether the key names a request a later authorize
// already replaced. Only the newest record is answerable verbatim, so an older
// key is unanswerable — and minting in its place would destroy the live flow it
// never named, which is the one thing an idempotency key exists to prevent.
func (p *providerAuth) requestRetired(key authFlowKey, requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, retired := p.retired[key][requestID]

	return retired
}

// retire records a request id the broker can no longer answer, for as long as
// the session lives. The caller holds the mutex.
func (p *providerAuth) retire(key authFlowKey, requestID string) {
	requests, ok := p.retired[key]
	if !ok {
		requests = make(map[string]struct{})
		p.retired[key] = requests
	}

	requests[requestID] = struct{}{}
}

// lineageCurrent reports whether the durable record still names exactly the
// binding this flow was minted against. A completion asks before it confirms
// anything: a disconnect that already bumped the generation released the slot,
// and confirming afterwards would leave the host reading `removed` while the
// credential this flow installed is resident and live. A record that cannot be
// read is no proof the binding survived, so it is not treated as one.
func (p *providerAuth) lineageCurrent(flow *authFlow) bool {
	record, ok, err := p.ledger.read(flow.providerID)

	return err == nil && ok &&
		record.ConnectionID == flow.connectionID &&
		record.Revision == flow.revision &&
		record.BindingGeneration == flow.bindingGeneration
}

// fenceLogins terminalizes every pending flow on this home and kills its login
// child. The harness arms its loopback hook unconditionally, so a live login
// child installs a credential the moment the operator's browser reaches it, with
// no leg driving it and nothing to serialize against: an absence proof taken
// while one is still running proves nothing past the instant it was read. The
// owner asked for the account to go, so the logins they own for it go with it.
func (p *providerAuth) fenceLogins() {
	p.mu.Lock()

	logins := make([]*authLoginHandle, 0, len(p.flows))

	for _, flow := range p.flows {
		if flow.state == authStateSaved {
			flow.dropCredential()
			flow.state = authStateCancelled
			flow.reason = authReasonOwnerCancel

			continue
		}

		if authTerminal(flow.state) {
			continue
		}

		flow.state = authStateCancelled
		flow.reason = authReasonOwnerCancel

		flow.stopCompleter()
		logins = append(logins, flow.takeLogin())
	}

	p.mu.Unlock()

	for _, login := range logins {
		login.close()
	}
}
