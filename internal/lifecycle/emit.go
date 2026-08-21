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
	// The verdicts mirror the decoder's: an unknown discriminant is the
	// discriminant's violation, a known one without its payload is shape.
	if !event.payloadMatchesType() {
		if !knownEventType(event.Type) {
			return nil, violation(ViolationUnknownEventType, s.id, s.sequence+1,
				"event type "+string(event.Type))
		}

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

// TransitionEvent reports one foreground transition that does not end its cycle.
// The cause is the origin of the turn holding the cycle: `submission` for a turn
// a prompt opened, `activity` for an agent-origin one, and a running transition
// naming a turn the stream has not introduced opens the latter. An ending idle is
// built by IdleEvent, which carries the recorded outcome.
func TransitionEvent(cause Cause, state ForegroundState, cycleID, turnID string) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:   state,
		CycleID: cycleID,
		TurnID:  turnID,
		Cause:   cause,
	}}
}

// IdleEvent ends one cycle, carrying the turn's truthful stop reason and recorded
// outcome under the same cause its opening transition carried.
func IdleEvent(cause Cause, cycleID, turnID, stopReason string, outcome Outcome) Event {
	return Event{Type: EventStateUpdate, State: &StateTransition{
		State:      ForegroundIdle,
		CycleID:    cycleID,
		TurnID:     turnID,
		Cause:      cause,
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

// encodeEvent renders every member of the closed lifecycle event set.
func encodeEvent(event Event) map[string]any {
	switch event.Type {
	case EventSnapshot:
		return encodeSnapshot(*event.Snapshot)
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
		return map[string]any{
			fieldType:   string(EventActionUpdate),
			fieldAction: encodeAction(*event.Action),
		}
	case EventActivityUpdate:
		return map[string]any{
			fieldType:     string(EventActivityUpdate),
			fieldActivity: encodeActivity(*event.Activity),
		}
	default:
		return encodeTransition(*event.State)
	}
}

// encodeSnapshot renders the whole state the assertion carries. The turn holding
// the foreground and that turn's origin are rendered exactly while one holds it,
// and the nonterminal sets are rendered member for member through the same
// encoders the delta events use. Fidelity here is what arms the reader: a member
// this step drops is a member the decode below never sees, so an assertion the
// adapter could not state truthfully would leave the emitter and pass every rule
// the foreground, the activity set, and the action set are held to.
func encodeSnapshot(snapshot Snapshot) map[string]any {
	foreground := map[string]any{
		fieldState:   string(snapshot.Foreground.State),
		fieldCycleID: snapshot.Foreground.CycleID,
	}
	withOptional(foreground, fieldTurnID, snapshot.Foreground.TurnID)
	withOptional(foreground, fieldOrigin, string(snapshot.Foreground.Origin))

	activities := make([]any, 0, len(snapshot.Activities))
	for index := range snapshot.Activities {
		activities = append(activities, encodeActivity(snapshot.Activities[index]))
	}

	actions := make([]any, 0, len(snapshot.Actions))
	for index := range snapshot.Actions {
		actions = append(actions, encodeAction(snapshot.Actions[index]))
	}

	return map[string]any{
		fieldType:       string(EventSnapshot),
		fieldForeground: foreground,
		fieldActivities: activities,
		fieldActions:    actions,
		fieldQuiescence: encodeQuiescence(snapshot.Quiescence),
	}
}

// encodeActivity renders exactly the members the caller stated. The identity and
// the state are what every sight of an activity carries, and everything else is
// rendered if and only if it has a value: a member the caller left unstated is
// omitted rather than rendered empty, because a rendered empty member is a
// statement, and the first sight that owes a complete identity is held to that by
// the reducer this render is read back through rather than by a render that
// fabricates the members it lacks.
func encodeActivity(activity ActivityUpdate) map[string]any {
	members := map[string]any{
		fieldActivityID: activity.ActivityID,
		fieldState:      string(activity.State),
	}
	withOptional(members, fieldKind, string(activity.Kind))
	withOptional(members, fieldCause, string(activity.Cause))
	withOptional(members, fieldOriginTurnID, activity.OriginTurnID)
	withOptional(members, fieldParentID, activity.ParentID)
	withOptional(members, fieldToolCallID, activity.ToolCallID)
	withOptional(members, fieldRunID, activity.RunID)

	if activity.Progress != nil {
		members[fieldProgress] = activity.Progress
	}

	return members
}

// encodeAction renders one action under the same rule. The blocking claim is the
// member that makes the rule load-bearing rather than tidy: it is a pointer
// precisely because an omitted claim and a stated false are different facts, so
// rendering an unstated one as false would fabricate a claim the caller never
// made — and a legal patch restating only the identity would then be refused as
// having changed what the action blocks.
func encodeAction(action ActionUpdate) map[string]any {
	members := map[string]any{
		fieldActionID: action.ActionID,
		fieldState:    string(action.State),
	}
	withOptional(members, fieldKind, string(action.Kind))

	if action.Owner != (Owner{}) {
		members[fieldOwner] = map[string]any{
			fieldType: string(action.Owner.Type),
			fieldID:   action.Owner.ID,
		}
	}

	if action.BlocksForeground != nil {
		members[fieldBlocksForeground] = *action.BlocksForeground
	}

	return withOptional(members, fieldRunID, action.RunID)
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
