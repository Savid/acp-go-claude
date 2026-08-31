package claudeacp

import (
	"encoding/json"

	"github.com/savid/acp-go-claude/internal/lifecycle"
)

// negotiateLifecycle reads the host's `acp-go.dev/lifecycle` offer and answers
// with the facts this connection's active configuration proved. It records the
// answer as the contract for the whole connection: with no capability, the key
// is omitted from the response and no envelope,
// correlation read, or lifecycle fact exists on the connection at all.
func (a *Agent) negotiateLifecycle(meta map[string]any) (map[string]any, error) {
	offered, refusal := lifecycle.DecodeCapability(meta)
	if refusal != nil {
		return nil, unsupportedField(refusal.Field)
	}

	if !offered || !a.lifecycleCarrierSupported() {
		a.retainNegotiatedLifecycle(lifecycle.Negotiated{})

		// An omitted key and an empty answer are the same wire fact: the
		// response carries no lifecycle member at all.
		return map[string]any{}, nil
	}

	answer := a.provenLifecycleFacts()
	answer.Version = lifecycle.Version

	a.retainNegotiatedLifecycle(answer)
	a.requireLifecycleWrites()

	return map[string]any{lifecycle.MetaKey: answer.Advertisement()}, nil
}

func (a *Agent) requireLifecycleWrites() {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()

	if local, ok := conn.(*localAgentConnection); ok {
		local.hooks.writes.requireInterruptible()
	}
}

// provenLifecycleFacts states what this configuration can actually prove, read
// from the same code path that enforces containment rather than from a
// compiled-in constant.
//
// The session owns the native reader and drains it from session start until the
// process ends, so notifications are delivered with no prompt in flight and none
// is dropped: `updatesOutsidePrompt` is true on every configuration. No captured
// native trace proves stable task identity and parentage, so no activity kind is
// advertised. Background native work is still represented by agent-origin turns
// and ordinary typed tool-call updates, without fabricating an activity registry.
// Only a supplied host authority owns the complete native tree, so only that
// configuration proves vacancy and names the `process-containment` class.
func (a *Agent) provenLifecycleFacts() lifecycle.Negotiated {
	proven := lifecycle.Negotiated{
		UpdatesOutsidePrompt: true,
		ActivityKinds:        []lifecycle.ActivityKind{},
	}
	if a.options.hostAuthoritySet {
		proven.AuthoritativeQuiescence = true
		proven.QuiescenceSource = lifecycle.ProofClassProcessContainment
	}

	return proven
}

func (a *Agent) retainNegotiatedLifecycle(answer lifecycle.Negotiated) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.lifecycle = answer
}

// negotiatedLifecycle reports the answer this connection is bound by.
func (a *Agent) negotiatedLifecycle() lifecycle.Negotiated {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.lifecycle
}

// readPromptCorrelation reads the submission identity a `session/prompt` carries.
// The value is required while version 1 is negotiated and forbidden otherwise, so
// both an absent one and a present one are refusals in the configuration that
// does not expect them, and both are answered before the prompt is dispatched.
func (a *Agent) readPromptCorrelation(meta map[string]any) (lifecycle.Submission, error) {
	submission, refusal := lifecycle.DecodePromptCorrelation(meta, a.negotiatedLifecycle())
	if refusal != nil {
		return lifecycle.Submission{}, unsupportedField(refusal.Field)
	}

	return submission, nil
}

// rejectLifecycleMeta refuses the lifecycle key on a surface that never carries
// it. A family literal is never foreign and never a no-op: an inbound surface
// outside `initialize`, `session/prompt`, and the lifecycle-bearing outbound
// surfaces rejects it rather than ignoring it as another namespace's business.
func rejectLifecycleMeta(meta map[string]any) error {
	if _, present := meta[lifecycle.MetaKey]; !present {
		return nil
	}

	return unsupportedField(lifecycle.MetaPath)
}

// rejectLifecycleExtensionMeta refuses the lifecycle key on an extension method's
// raw params, at the dispatch boundary and ahead of every leg's own closed-member
// validation. The legs reject an unknown `_meta` by that member's name, which
// names the object rather than the reserved literal inside it, and a caller is
// owed the exact path of the family key it sent.
func rejectLifecycleExtensionMeta(params json.RawMessage) error {
	var envelope struct {
		Meta map[string]json.RawMessage `json:"_meta"` //nolint:tagliatelle // ACP reserves this exact wire member.
	}

	if err := json.Unmarshal(params, &envelope); err != nil {
		// A params object this step cannot read is the method's own business: it
		// carries no readable reserved key, so dispatch continues and the leg
		// answers with its own refusal.
		return nil //nolint:nilerr // The method-specific decoder owns malformed params.
	}

	if _, present := envelope.Meta[lifecycle.MetaKey]; !present {
		return nil
	}

	return unsupportedField(lifecycle.MetaPath)
}
