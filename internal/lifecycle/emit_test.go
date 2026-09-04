package lifecycle

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// provenConfiguration is the answer a configuration whose close containment
// proves whole-tree vacancy gives: a channel outside a prompt, no activity kind,
// and the process-containment proof class.
func provenConfiguration() Negotiated {
	return Negotiated{
		Version:                 Version,
		UpdatesOutsidePrompt:    true,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{},
	}
}

// openStream opens an incarnation ready to emit deltas.
func openStream(t *testing.T, negotiated Negotiated, id string) *Stream {
	t.Helper()

	stream := NewStream(negotiated)
	stream.Incarnate(id)

	_, err := stream.Emit(SnapshotEvent("cyc-0", QuiescenceFact{}))
	require.NoError(t, err)

	return stream
}

// notificationFor renders the notification an envelope rides, exactly as the
// carrier this adapter delivers it on.
func notificationFor(t *testing.T, envelope map[string]any) json.RawMessage {
	t.Helper()

	params, err := json.Marshal(map[string]any{
		"sessionId": "sess-1",
		updateField: map[string]any{sessionUpdateField: string(CarrierSessionInfo)},
		metaField:   map[string]any{MetaKey: envelope},
	})
	require.NoError(t, err)

	return params
}

// TestEmittedStreamReducesThroughTheSameReducer proves the emitted bytes are
// wire-legal by the only measure that counts: decoding them from a
// session/update notification and reducing them through the reducer the family
// battery drives.
func TestEmittedStreamReducesThroughTheSameReducer(t *testing.T) {
	t.Parallel()

	negotiated := provenConfiguration()
	stream := NewStream(negotiated)
	stream.Incarnate("strm-1")

	submission := Submission{SubmissionID: "sub-1", ClientNonce: "non-1", RunID: "run-1"}
	envelopes := make([]map[string]any, 0, 5)

	for _, event := range []Event{
		SnapshotEvent("cyc-0", QuiescenceFact{}),
		AcceptedEvent(submission, "turn-1"),
		TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"),
		IdleEvent(CauseSubmission, "cyc-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
	} {
		envelope, err := stream.Emit(event)
		require.NoError(t, err)

		envelopes = append(envelopes, envelope)
	}

	settled, err := stream.Emit(QuiescenceEvent(QuiescenceFact{
		Quiescent: true,
		Source:    ProofClassProcessContainment,
		Watermark: stream.State().ReducedThrough,
		Barrier:   "contained-exit-1",
	}))
	require.NoError(t, err)

	envelopes = append(envelopes, settled)
	reducer := NewReducer(Options{Negotiated: negotiated})

	for index, envelope := range envelopes {
		require.NoError(t, reducer.ReduceSessionUpdate(notificationFor(t, envelope)), "envelope %d", index)
	}

	state := reducer.State()
	require.Equal(t, "strm-1", state.StreamID)
	require.Equal(t, uint64(5), state.ReducedThrough)
	require.Equal(t, []TurnRecord{{
		TurnID:       "turn-1",
		Origin:       CauseSubmission,
		Terminal:     true,
		Outcome:      OutcomeSuccess,
		SubmissionID: "sub-1",
		ClientNonce:  "non-1",
		RunID:        "run-1",
		CycleID:      "cyc-1",
		StopReason:   StopReasonEndTurn,
	}}, state.Turns)
	require.True(t, state.Quiescence.Certified)
	require.Equal(t, uint64(4), state.Quiescence.Watermark)
	require.Equal(t, "contained-exit-1", state.Quiescence.Barrier)
}

// TestEmitClaimsTheSequenceBeforeDelivery proves a refused event consumes its
// sequence: a counter that advanced only on success would make loss invisible,
// which is the exact failure contiguity exists to expose.
func TestEmitClaimsTheSequenceBeforeDelivery(t *testing.T) {
	t.Parallel()

	stream := NewStream(provenConfiguration())
	stream.Incarnate("strm-1")

	_, err := stream.Emit(TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"))
	require.ErrorAs(t, err, new(*ViolationError))
	require.Equal(t, uint64(1), stream.sequence)
}

// TestEmitterRefusesAStructurallyInvalidIdentity proves emitter input passes the
// same structural validation as decoded wire input: an empty opaque identifier is
// a malformed envelope at the point of emission, not a stream a consumer has to
// refuse.
func TestEmitterRefusesAStructurallyInvalidIdentity(t *testing.T) {
	t.Parallel()

	stream := openStream(t, provenConfiguration(), "strm-1")
	blocks := true

	_, err := stream.Emit(ActionEvent(ActionUpdate{
		Kind:             ActionPermission,
		State:            ActionPending,
		Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
		BlocksForeground: &blocks,
	}))

	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationMalformedEnvelope, refusal.Kind)
}

// TestEmitterRefusesAnUnnegotiatedFact proves the answer is the contract for the
// connection: a configuration that proved no class cannot emit a positive
// quiescence fact even where its own boundary completed.
func TestEmitterRefusesAnUnnegotiatedFact(t *testing.T) {
	t.Parallel()

	degenerate := Negotiated{Version: Version, ActivityKinds: []ActivityKind{}}
	stream := openStream(t, degenerate, "strm-1")

	_, err := stream.Emit(QuiescenceEvent(QuiescenceFact{
		Quiescent: true,
		Source:    ProofClassProcessContainment,
	}))

	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationUnnegotiatedFact, refusal.Kind)
}

// TestSnapshotStatesAnUnprovenBoundaryAsNotQuiescent proves a configuration with
// no proof class emits a negative fact rather than a `none` sentinel or a
// present-and-empty source.
func TestSnapshotStatesAnUnprovenBoundaryAsNotQuiescent(t *testing.T) {
	t.Parallel()

	degenerate := Negotiated{Version: Version, ActivityKinds: []ActivityKind{}}
	stream := NewStream(degenerate)
	stream.Incarnate("strm-1")

	envelope, err := stream.Emit(SnapshotEvent("cyc-0", QuiescenceFact{}))
	require.NoError(t, err)

	event, ok := envelope[fieldEvent].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{fieldQuiescent: false}, event[fieldQuiescence])
}

// TestAcceptanceOmitsAnAbsentRunID proves an optional handle is omitted rather
// than emitted empty: an empty opaque identifier fails closed on the reader.
func TestAcceptanceOmitsAnAbsentRunID(t *testing.T) {
	t.Parallel()

	stream := openStream(t, provenConfiguration(), "strm-1")

	envelope, err := stream.Emit(AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "turn-1"))
	require.NoError(t, err)

	event, ok := envelope[fieldEvent].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, event, fieldRunID)
}

// TestEmittedActionStreamReducesThroughTheSameReducer proves the action shapes
// this adapter emits — a blocking action's first sight, its terminal patch, and
// the transitions that accompany them — survive serialization and a strict
// re-decode into an independent reducer.
func TestEmittedActionStreamReducesThroughTheSameReducer(t *testing.T) {
	t.Parallel()

	negotiated := provenConfiguration()
	stream := NewStream(negotiated)
	stream.Incarnate("strm-1")

	owner := Owner{Type: OwnerTurn, ID: "turn-1"}
	blocks := true
	pending := ActionUpdate{
		ActionID:         "act-1",
		Kind:             ActionPermission,
		State:            ActionPending,
		Owner:            owner,
		RunID:            "run-1",
		BlocksForeground: &blocks,
	}
	accepted := pending
	accepted.State = ActionAccepted
	correlation := ActionCorrelationValue("strm-1", pending)
	require.Equal(t, map[string]any{
		fieldVersion:  1,
		fieldStreamID: "strm-1",
		fieldAction: map[string]any{
			fieldActionID: "act-1",
			fieldOwner:    map[string]any{fieldType: string(OwnerTurn), fieldID: "turn-1"},
			fieldRunID:    "run-1",
		},
	}, correlation)

	envelopes := make([]map[string]any, 0, 8)

	for _, event := range []Event{
		SnapshotEvent("cyc-0", QuiescenceFact{}),
		AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "non-1", RunID: "run-1"}, "turn-1"),
		TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"),
		ActionEvent(pending),
		TransitionEvent(CauseSubmission, ForegroundRequiresAction, "cyc-1", "turn-1"),
		ActionEvent(accepted),
		TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"),
		IdleEvent(CauseSubmission, "cyc-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
	} {
		envelope, err := stream.Emit(event)
		require.NoError(t, err)

		envelopes = append(envelopes, envelope)
	}

	reducer := NewReducer(Options{Negotiated: negotiated})
	for index, envelope := range envelopes {
		require.NoError(t, reducer.ReduceSessionUpdate(notificationFor(t, envelope)), "envelope %d", index)
	}

	state := reducer.State()
	require.Equal(t, []ActionRecord{{
		ActionID:         "act-1",
		Kind:             ActionPermission,
		State:            ActionAccepted,
		Owner:            owner,
		RunID:            "run-1",
		BlocksForeground: true,
	}}, state.Actions)
	require.Equal(t, ForegroundIdle, state.Foreground.State)
}

// TestEmittedActionPatchesStateOnlyWhatTheCallerStated proves the render is
// patch-faithful. A patch is legal precisely because it may restate a subset of
// an action's members, and a render that filled the rest in from zero values
// would turn every such patch into a lie the emit gate then refuses: an unstated
// blocking claim rendered as false reads as a change to what the action blocks,
// and an unstated kind rendered as "" is a malformed envelope. Both shapes below
// are wire-legal, so both must leave this emitter and reduce.
func TestEmittedActionPatchesStateOnlyWhatTheCallerStated(t *testing.T) {
	t.Parallel()

	owner := Owner{Type: OwnerTurn, ID: "turn-1"}
	blocks := true
	first := ActionUpdate{
		ActionID:         "act-1",
		Kind:             ActionPermission,
		State:            ActionPending,
		Owner:            owner,
		BlocksForeground: &blocks,
	}

	for _, testCase := range []struct {
		name  string
		patch ActionUpdate
		want  map[string]any
	}{
		{
			name: "a patch restating the identity but not the blocking claim",
			patch: ActionUpdate{
				ActionID: "act-1",
				Kind:     ActionPermission,
				State:    ActionAccepted,
				Owner:    owner,
			},
			want: map[string]any{
				fieldActionID: "act-1",
				fieldKind:     string(ActionPermission),
				fieldState:    string(ActionAccepted),
				fieldOwner:    map[string]any{fieldType: string(OwnerTurn), fieldID: "turn-1"},
			},
		},
		{
			name:  "a patch stating the identity and the new state alone",
			patch: ActionUpdate{ActionID: "act-1", State: ActionAccepted},
			want: map[string]any{
				fieldActionID: "act-1",
				fieldState:    string(ActionAccepted),
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			negotiated := provenConfiguration()
			stream := NewStream(negotiated)
			stream.Incarnate("strm-1")

			envelopes := make([]map[string]any, 0, 5)

			for _, event := range []Event{
				SnapshotEvent("cyc-0", QuiescenceFact{}),
				AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "non-1"}, "turn-1"),
				TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"),
				ActionEvent(first),
				TransitionEvent(CauseSubmission, ForegroundRequiresAction, "cyc-1", "turn-1"),
			} {
				envelope, err := stream.Emit(event)
				require.NoError(t, err)

				envelopes = append(envelopes, envelope)
			}

			patched, err := stream.Emit(ActionEvent(testCase.patch))
			require.NoError(t, err, "a legal patch is emittable")
			event, ok := patched[fieldEvent].(map[string]any)
			require.True(t, ok)
			require.Equal(t, testCase.want, event[fieldAction])

			envelopes = append(envelopes, patched)

			reducer := NewReducer(Options{Negotiated: negotiated})
			for index, envelope := range envelopes {
				require.NoError(t, reducer.ReduceSessionUpdate(notificationFor(t, envelope)), "envelope %d", index)
			}

			require.Equal(t, []ActionRecord{{
				ActionID:         "act-1",
				Kind:             ActionPermission,
				State:            ActionAccepted,
				Owner:            owner,
				BlocksForeground: true,
			}}, reducer.State().Actions, "an unstated member leaves the reduced record as it stood")
		})
	}
}

// TestEmittedActivityPatchesStateOnlyWhatTheCallerStated holds the activity
// render to the same rule. This adapter proves no activity kind and emits none
// today, but the render is also the emitter's validator, so a render that
// fabricated kind, cause, or origin would make an unemittable patch look like a
// caller defect.
func TestEmittedActivityPatchesStateOnlyWhatTheCallerStated(t *testing.T) {
	t.Parallel()

	require.Equal(t, map[string]any{
		fieldActivityID: "acv-1",
		fieldState:      string(ActivityCompleted),
	}, encodeActivity(ActivityUpdate{ActivityID: "acv-1", State: ActivityCompleted}))

	require.Equal(t, map[string]any{
		fieldActivityID:   "acv-1",
		fieldState:        string(ActivityRunning),
		fieldKind:         string(ActivityTask),
		fieldCause:        string(CauseSubmission),
		fieldOriginTurnID: "turn-1",
	}, encodeActivity(ActivityUpdate{
		ActivityID:   "acv-1",
		State:        ActivityRunning,
		Kind:         ActivityTask,
		Cause:        CauseSubmission,
		OriginTurnID: "turn-1",
	}))
}

// TestEmitterRefusesAMisshapenSnapshotForeground proves the emitter is held to
// every rule the decoder states about a snapshot's foreground. The rendering is
// what is validated, so a foreground member the encoder dropped would be a rule
// the emit gate could never enforce: each case below is legal only to an emitter
// that never states the turn and the origin it is refused for.
func TestEmitterRefusesAMisshapenSnapshotForeground(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		foreground Foreground
		detail     string
	}{
		{
			name:       "an idle foreground naming a turn",
			foreground: Foreground{State: ForegroundIdle, CycleID: "cyc-1", TurnID: "turn-1", Origin: CauseSubmission},
			detail:     "an idle foreground reports no turn",
		},
		{
			name:       "a turn without its origin",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cyc-1", TurnID: "turn-1"},
			detail:     "foreground origin is present exactly while a turn is",
		},
		{
			name:       "an origin without its turn",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cyc-1", Origin: CauseSubmission},
			detail:     "foreground origin is present exactly while a turn is",
		},
		{
			name:       "a session-caused origin",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cyc-1", TurnID: "turn-1", Origin: CauseSession},
			detail:     "foreground origin session",
		},
		{
			name:       "an out-of-vocabulary origin",
			foreground: Foreground{State: ForegroundRunning, CycleID: "cyc-1", TurnID: "turn-1", Origin: Cause("resumed")},
			detail:     "foreground origin resumed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			stream := NewStream(provenConfiguration())
			stream.Incarnate("strm-1")

			_, err := stream.Emit(Event{Type: EventSnapshot, Snapshot: &Snapshot{Foreground: testCase.foreground}})

			var refusal *ViolationError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, ViolationMalformedEnvelope, refusal.Kind)
			require.Equal(t, testCase.detail, refusal.Detail)
		})
	}
}

// TestEmittedMidTurnSnapshotRoundTripsWholly proves the opening assertion carries
// the whole state it claims: an independent reducer reading the emitted envelope
// through this package's own decoder reaches exactly the projection the emitter
// proved, with the held turn, the live activity, and the pending blocking action
// all present rather than silently dropped by the rendering.
func TestEmittedMidTurnSnapshotRoundTripsWholly(t *testing.T) {
	t.Parallel()

	negotiated := provenConfiguration()
	negotiated.ActivityKinds = []ActivityKind{ActivityTask}

	stream := NewStream(negotiated)
	stream.Incarnate("strm-1")

	blocks := true
	progress := json.RawMessage(`{"phase":"resumed"}`)
	foreground := Foreground{
		State:   ForegroundRunning,
		CycleID: "cyc-1",
		TurnID:  "turn-1",
		Origin:  CauseSubmission,
	}

	envelope, err := stream.Emit(Event{Type: EventSnapshot, Snapshot: &Snapshot{
		Foreground: foreground,
		Activities: []ActivityUpdate{{
			ActivityID:   "acv-1",
			Kind:         ActivityTask,
			State:        ActivityRunning,
			ToolCallID:   "tool-1",
			Cause:        CauseSubmission,
			OriginTurnID: "turn-1",
			RunID:        "run-1",
			Progress:     progress,
		}},
		Actions: []ActionUpdate{{
			ActionID:         "act-1",
			Kind:             ActionPermission,
			State:            ActionPending,
			Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
			RunID:            "run-1",
			BlocksForeground: &blocks,
		}},
	}})
	require.NoError(t, err)

	reducer := NewReducer(Options{Negotiated: negotiated})
	require.NoError(t, reducer.ReduceSessionUpdate(notificationFor(t, envelope)))

	read := reducer.State()
	require.Equal(t, stream.State(), read)
	require.Equal(t, &foreground, read.Foreground)
	require.Equal(t, []TurnRecord{{
		TurnID:  "turn-1",
		Origin:  CauseSubmission,
		CycleID: "cyc-1",
	}}, read.Turns)
	require.Equal(t, []ActivityRecord{{
		ActivityID:   "acv-1",
		Kind:         ActivityTask,
		State:        ActivityRunning,
		ToolCallID:   "tool-1",
		Cause:        CauseSubmission,
		OriginTurnID: "turn-1",
		RunID:        "run-1",
		Progress:     progress,
	}}, read.Activities)
	require.Equal(t, []ActionRecord{{
		ActionID:         "act-1",
		Kind:             ActionPermission,
		State:            ActionPending,
		Owner:            Owner{Type: OwnerTurn, ID: "turn-1"},
		RunID:            "run-1",
		BlocksForeground: true,
	}}, read.Actions)
	require.False(t, read.Quiescence.Certified)
}

// TestFenceEndsTheSessionForEveryIncarnation proves close fences the session
// rather than the stream identity it fenced: the next incarnation's own opening
// snapshot is stale.
func TestFenceEndsTheSessionForEveryIncarnation(t *testing.T) {
	t.Parallel()

	stream := openStream(t, provenConfiguration(), "strm-1")

	stream.Incarnate("strm-2")

	_, err := stream.Emit(SnapshotEvent("cyc-0", QuiescenceFact{}))
	require.NoError(t, err, "a surviving session admits the next incarnation")

	stream.Fence()
	stream.Incarnate("strm-3")

	_, err = stream.Emit(SnapshotEvent("cyc-0", QuiescenceFact{}))

	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationStaleStream, refusal.Kind)
}

// TestEmitterRefusesAnEventOnASupersededIncarnation proves a rotated identity is
// terminal for the incarnation it replaced.
func TestEmitterRefusesAnEventOnASupersededIncarnation(t *testing.T) {
	t.Parallel()

	negotiated := provenConfiguration()
	stream := openStream(t, negotiated, "strm-1")
	superseded := stream.sequence

	stream.Incarnate("strm-2")

	_, err := stream.Emit(SnapshotEvent("cyc-0", QuiescenceFact{}))
	require.NoError(t, err)

	stream.id = "strm-1"
	stream.sequence = superseded

	_, err = stream.Emit(TransitionEvent(CauseSubmission, ForegroundRunning, "cyc-1", "turn-1"))

	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationStaleStream, refusal.Kind)
}

// TestEmitRefusesAPayloadMismatchedEvent pins that a type/payload mismatch is
// refused as a malformed envelope before any sequence is claimed, for every
// event form, instead of dereferencing a payload that is not there.
func TestEmitRefusesAPayloadMismatchedEvent(t *testing.T) {
	t.Parallel()

	stream := openStream(t, provenConfiguration(), "strm-1")

	for _, event := range []Event{
		{Type: EventSnapshot},
		{Type: EventPromptAccepted},
		{Type: EventStateUpdate},
		{Type: EventActivityUpdate},
		{Type: EventActionUpdate},
		{Type: EventQuiescenceUpdate},
	} {
		_, err := stream.Emit(event)
		var refusal *ViolationError
		require.ErrorAs(t, err, &refusal, "event %s", event.Type)
		require.Equal(t, ViolationMalformedEnvelope, refusal.Kind)
	}

	// An unknown discriminant reports the decoder's own verdict for it.
	_, err := stream.Emit(Event{Type: EventType("unknown")})
	var unknown *ViolationError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, ViolationUnknownEventType, unknown.Kind)

	// No refused emit burned a sequence: the next legal delta still lands.
	_, err = stream.Emit(Event{Type: EventQuiescenceUpdate, Quiescence: &QuiescenceFact{
		Quiescent: true, Source: ProofClassProcessContainment, Watermark: 1,
	}})
	require.NoError(t, err)
}

// TestEmittedActivityRendersWholeAndAnswersToTheNegotiatedFacts proves an
// activity event is encoded member for member rather than falling into the
// transition arm, and that this configuration's reducer still refuses the fact
// it never advertised.
func TestEmittedActivityRendersWholeAndAnswersToTheNegotiatedFacts(t *testing.T) {
	t.Parallel()

	activity := ActivityUpdate{
		ActivityID: "act-1", Kind: ActivityTask, State: ActivityRunning,
		Cause: CauseSubmission, OriginTurnID: "turn-1", RunID: "run-1",
	}

	rendered := encodeEvent(Event{Type: EventActivityUpdate, Activity: &activity})
	require.Equal(t, string(EventActivityUpdate), rendered[fieldType])
	member, ok := rendered[fieldActivity].(map[string]any)
	require.True(t, ok, "the activity event carries its member, not a transition")
	require.Equal(t, "act-1", member[fieldActivityID])

	stream := openStream(t, provenConfiguration(), "strm-1")
	_, err := stream.Emit(Event{Type: EventActivityUpdate, Activity: &activity})
	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationUnnegotiatedFact, refusal.Kind)
}
