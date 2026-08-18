package claudeacp

import (
	"github.com/savid/acp-go-claude/internal/lifecycle"
)

// negotiateLifecycle reads the host's `acp-go.dev/lifecycle` offer and answers
// with the facts this connection's active configuration proved. It records the
// answer as the contract for the whole connection: with no offer, or with no
// common version, the key is omitted from the response and no envelope,
// correlation read, or lifecycle fact exists on the connection at all.
func (a *Agent) negotiateLifecycle(meta map[string]any) (map[string]any, error) {
	offer, offered, refusal := lifecycle.DecodeOffer(meta)
	if refusal != nil {
		return nil, unsupportedField(refusal.Field)
	}

	answer, common := offer.Answer(a.provenLifecycleFacts())
	if !offered || !common {
		a.retainNegotiatedLifecycle(lifecycle.Negotiated{})

		// An omitted key and an empty answer are the same wire fact: the
		// response carries no lifecycle member at all.
		return map[string]any{}, nil
	}

	a.retainNegotiatedLifecycle(answer)

	return map[string]any{lifecycle.MetaKey: answer.Advertisement()}, nil
}

// provenLifecycleFacts states what this configuration can actually prove, read
// from the same code path that enforces containment rather than from a
// compiled-in constant.
//
// Nobody consumes the native stream between prompts, so there is no channel
// outside a prompt and no proven activity kind: `updatesOutsidePrompt` is false
// and `activityKinds` is empty on every configuration. Only the authoritative
// containment mode enumerates the whole descendant tree, so only it proves
// vacancy and names the `process-containment` class; ordinary same-identity
// execution and opted-in Darwin containment prove a weaker boundary and a
// weaker boundary is never promoted.
func (a *Agent) provenLifecycleFacts() lifecycle.Negotiated {
	proven := lifecycle.Negotiated{ActivityKinds: []lifecycle.ActivityKind{}}
	if a.containmentMode.provesWholeTreeLifecycle() {
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
