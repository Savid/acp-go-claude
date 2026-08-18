package lifecycle

import "encoding/json"

// Stream is one session's ordered emitter. It follows the session across
// incarnations: Incarnate opens the next one, and Fence ends the session so no
// later incarnation of it exists.
//
// Every emission is rendered as the notification it will ride and then read back
// through DecodeSessionUpdate before it is reduced, so emitter input passes the
// same structural, carrier, and ordering validation as decoded wire input and an
// event this adapter could not state truthfully fails at the point of emission.
// The sequence is claimed before delivery is attempted, so a refused or lost
// event leaves a detectable gap.
//
// A Stream is not safe for concurrent use; its owner serializes emission and the
// state the emitted events report.
type Stream struct {
	reducer  *Reducer
	id       string
	sequence uint64
}

// NewStream opens an emitter for one session's whole life.
func NewStream(negotiated Negotiated) *Stream {
	return &Stream{reducer: NewReducer(Options{Negotiated: negotiated})}
}

// Incarnate names the next incarnation. The identity names one native lifecycle
// source lifetime: it never rotates while that source survives, and it never
// outlives it. Each incarnation carries its own sequence space and its own
// entities.
func (s *Stream) Incarnate(id string) {
	s.id = id
	s.sequence = 0
}

// ID reports the incarnation this stream currently speaks for.
func (s *Stream) ID() string { return s.id }

// State returns the projection the emitted stream proves.
func (s *Stream) State() State { return s.reducer.State() }

// Fence records that the session's close containment completed. The session is
// over: a later event on it, an opening snapshot for a would-be new incarnation
// included, fails closed as stale.
func (s *Stream) Fence() { s.reducer.Close() }

// Emit claims the next sequence, validates and reduces the notification the
// envelope will ride, and returns the envelope for that notification's `_meta`.
func (s *Stream) Emit(event Event) (map[string]any, error) {
	// The payload is judged before the sequence claim, so a caller defect
	// neither burns a sequence nor dereferences a payload that is not there.
	if !event.payloadMatchesType() {
		return nil, violation(ViolationMalformedEnvelope, s.id, s.sequence+1,
			"event payload does not match type "+string(event.Type))
	}

	s.sequence++

	envelope := map[string]any{
		fieldVersion:  Version,
		fieldStreamID: s.id,
		fieldSequence: s.sequence,
		fieldEvent:    encodeEvent(event),
	}

	// A rendered envelope holds only JSON-safe values, and a payload this step
	// could not produce fails the decode below as a malformed envelope rather
	// than escaping as an untyped error.
	params, _ := json.Marshal(map[string]any{
		metaField:   map[string]any{MetaKey: envelope},
		updateField: map[string]any{sessionUpdateField: string(CarrierSessionInfo)},
	})

	delivery, err := DecodeSessionUpdate(params, s.reducer.Negotiated())
	if err != nil {
		return nil, err
	}

	if err := s.reducer.Reduce(delivery); err != nil {
		return nil, err
	}

	return envelope, nil
}

// SnapshotEvent opens an incarnation from the whole state this adapter can state
// truthfully. A fresh incarnation holds nothing live, so the nonterminal sets are
// empty and the quiescence fact is whatever proof class actually completed
// before it.
func SnapshotEvent(cycleID string, quiescence QuiescenceFact) Event {
	return Event{Type: EventSnapshot, Snapshot: &Snapshot{
		Foreground: Foreground{State: ForegroundIdle, CycleID: cycleID},
		Quiescence: quiescence,
	}}
}

// AcceptedEvent records that the native dispatcher took durable ownership of a
// submitted frame. The submission identity is echoed verbatim from the prompt's
// correlation value.
func AcceptedEvent(submission Submission, turnID string) Event {
	return Event{Type: EventPromptAccepted, PromptAccepted: &PromptAccepted{
		SubmissionID: submission.SubmissionID,
		ClientNonce:  submission.ClientNonce,
		TurnID:       turnID,
		RunID:        submission.RunID,
	}}
}

// TransitionEvent reports one foreground transition of a submission-caused cycle
// that does not end it. An ending idle is built by IdleEvent, which carries the
// recorded outcome.
func TransitionEvent(state ForegroundState, cycleID, turnID string) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:   state,
		CycleID: cycleID,
		TurnID:  turnID,
		Cause:   CauseSubmission,
	}}
}

// IdleEvent ends the cycle a submission caused, carrying the turn's truthful stop
// reason and recorded outcome.
func IdleEvent(cycleID, turnID, stopReason string, outcome Outcome) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:      ForegroundIdle,
		CycleID:    cycleID,
		TurnID:     turnID,
		Cause:      CauseSubmission,
		StopReason: stopReason,
		Outcome:    outcome,
	}}
}

// QuiescenceEvent states the authoritative quiescence fact a completed proof
// produced. It carries the proof class and the watermark that proof covers, never
// a guess, a heuristic, or a confidence.
func QuiescenceEvent(fact QuiescenceFact) Event {
	return Event{Type: EventQuiescenceUpdate, Quiescence: &fact}
}

// ActionEvent reports one permission or elicitation action's state. The emitter
// restates the immutable identity on every patch: a restated member carrying its
// first-sight value is legal on either sight.
func ActionEvent(action ActionUpdate) Event {
	return Event{Type: EventActionUpdate, Action: &action}
}

// ActionCorrelationValue renders the value stamped on a
// session/request_permission or elicitation/create while version 1 is
// negotiated: the emitting stream's identity and the action's real owner, so the
// pending request has a stable lifecycle name beside its routing envelope.
func ActionCorrelationValue(streamID string, action ActionUpdate) map[string]any {
	member := map[string]any{
		fieldActionID: action.ActionID,
		fieldOwner: map[string]any{
			fieldType: string(action.Owner.Type),
			fieldID:   action.Owner.ID,
		},
	}

	return map[string]any{
		fieldVersion:  Version,
		fieldStreamID: streamID,
		fieldAction:   withOptional(member, fieldRunID, action.RunID),
	}
}

// encodeEvent renders the events this adapter emits. It proves no activity kind,
// so activity_update has no emitter here; the reducer still reduces all six,
// because it is also the validator for streams this adapter reads.
func encodeEvent(event Event) map[string]any {
	switch event.Type {
	case EventSnapshot:
		return map[string]any{
			fieldType: string(EventSnapshot),
			fieldForeground: map[string]any{
				fieldState:   string(event.Snapshot.Foreground.State),
				fieldCycleID: event.Snapshot.Foreground.CycleID,
			},
			fieldActivities: []any{},
			fieldActions:    []any{},
			fieldQuiescence: encodeQuiescence(event.Snapshot.Quiescence),
		}
	case EventPromptAccepted:
		return withOptional(map[string]any{
			fieldType:         string(EventPromptAccepted),
			fieldSubmissionID: event.PromptAccepted.SubmissionID,
			fieldClientNonce:  event.PromptAccepted.ClientNonce,
			fieldTurnID:       event.PromptAccepted.TurnID,
		}, fieldRunID, event.PromptAccepted.RunID)
	case EventQuiescenceUpdate:
		fact := encodeQuiescence(*event.Quiescence)
		fact[fieldType] = string(EventQuiescenceUpdate)

		return fact
	case EventActionUpdate:
		return encodeAction(*event.Action)
	default:
		return encodeTransition(*event.State)
	}
}

func encodeAction(action ActionUpdate) map[string]any {
	members := map[string]any{
		fieldActionID: action.ActionID,
		fieldKind:     string(action.Kind),
		fieldState:    string(action.State),
		fieldOwner: map[string]any{
			fieldType: string(action.Owner.Type),
			fieldID:   action.Owner.ID,
		},
		fieldBlocksForeground: action.BlocksForeground != nil && *action.BlocksForeground,
	}

	return map[string]any{
		fieldType:   string(EventActionUpdate),
		fieldAction: withOptional(members, fieldRunID, action.RunID),
	}
}

func encodeTransition(transition StateTransition) map[string]any {
	encoded := map[string]any{
		fieldType:    string(EventStateUpdate),
		fieldState:   string(transition.State),
		fieldCycleID: transition.CycleID,
		fieldTurnID:  transition.TurnID,
		fieldCause:   string(transition.Cause),
	}
	withOptional(encoded, fieldStopReason, transition.StopReason)
	withOptional(encoded, fieldOutcome, string(transition.Outcome))

	return encoded
}

// encodeQuiescence renders a fact's members. A negative fact carries no proof at
// all: `source` is present if and only if the fact is positive, and it is never a
// `none` sentinel.
func encodeQuiescence(fact QuiescenceFact) map[string]any {
	if !fact.Quiescent {
		return map[string]any{fieldQuiescent: false}
	}

	encoded := map[string]any{
		fieldQuiescent: true,
		fieldSource:    string(fact.Source),
		fieldWatermark: fact.Watermark,
	}

	return withOptional(encoded, fieldBarrier, fact.Barrier)
}

// withOptional adds a member only when it has a value. An optional member is
// omitted rather than emitted empty, because an empty opaque identifier fails
// closed on the reading side.
func withOptional(encoded map[string]any, key, value string) map[string]any {
	if value != "" {
		encoded[key] = value
	}

	return encoded
}
