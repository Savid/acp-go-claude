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
		Versions:                []int{Version},
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
		TransitionEvent(ForegroundRunning, "cyc-1", "turn-1"),
		IdleEvent("cyc-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
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

	_, err := stream.Emit(TransitionEvent(ForegroundRunning, "cyc-1", "turn-1"))
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

	degenerate := Negotiated{Versions: []int{Version}, ActivityKinds: []ActivityKind{}}
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

	degenerate := Negotiated{Versions: []int{Version}, ActivityKinds: []ActivityKind{}}
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

	envelopes := make([]map[string]any, 0, 8)

	for _, event := range []Event{
		SnapshotEvent("cyc-0", QuiescenceFact{}),
		AcceptedEvent(Submission{SubmissionID: "sub-1", ClientNonce: "non-1", RunID: "run-1"}, "turn-1"),
		TransitionEvent(ForegroundRunning, "cyc-1", "turn-1"),
		ActionEvent(pending),
		TransitionEvent(ForegroundRequiresAction, "cyc-1", "turn-1"),
		ActionEvent(accepted),
		TransitionEvent(ForegroundRunning, "cyc-1", "turn-1"),
		IdleEvent("cyc-1", "turn-1", StopReasonEndTurn, OutcomeSuccess),
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

	_, err = stream.Emit(TransitionEvent(ForegroundRunning, "cyc-1", "turn-1"))

	var refusal *ViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, ViolationStaleStream, refusal.Kind)
}
