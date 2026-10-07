package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func compactionReports(t *testing.T, notifications []acp.SessionNotification) []wire.Compaction {
	t.Helper()
	var reports []wire.Compaction
	for _, notification := range notifications {
		value, exists := notification.Meta[wire.CompactionKey]
		if !exists {
			continue
		}
		carrier, err := json.Marshal(notification.Update)
		require.NoError(t, err)
		require.JSONEq(t, `{"sessionUpdate":"session_info_update"}`, string(carrier))
		require.Len(t, notification.Meta, 1)
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		var report wire.Compaction
		require.NoError(t, json.Unmarshal(encoded, &report))
		require.NotEmpty(t, report.CompactionID)
		reports = append(reports, report)
	}

	return reports
}

func TestCompactionTransport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize()
	created := h.newSession()
	for range 2 {
		_, err := h.prompt(created.SessionId, "COMPACT", nil)
		require.NoError(t, err)
	}
	reports := compactionReports(t, h.rec.snapshot())
	require.Len(t, reports, 2)
	require.NotEqual(t, reports[0].CompactionID, reports[1].CompactionID)
	for _, report := range reports {
		require.Equal(t, wire.CompactionCompleted, report.Status)
		require.Equal(t, "auto", report.Trigger)
		require.Equal(t, new(950), report.ContextBefore)
		require.Equal(t, new(120), report.ContextAfter)
	}
}

func TestCompactionNativeBoundaryAndReplay(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	c := &cycle{}
	event := claude.Event{Type: nativeSystem, Subtype: nativeCompactBoundary, UUID: "first", CompactMetadata: &claude.CompactMetadata{Trigger: "manual", PreTokens: new(0), PostTokens: new(0)}}
	for range 2 {
		_, err := s.projectEvent(t.Context(), nil, c, event)
		require.NoError(t, err)
	}
	event.ParentToolUseID = "child"
	event.UUID = "child-boundary"
	_, err := s.projectEvent(t.Context(), nil, c, event)
	require.NoError(t, err)
	event.ParentToolUseID = ""
	event.UUID = "next"
	event.CompactMetadata = nil
	_, err = s.projectEvent(t.Context(), nil, c, event)
	require.NoError(t, err)
	reports := compactionReports(t, rec.snapshot())
	require.Len(t, reports, 2)
	require.Equal(t, "manual", reports[0].Trigger)
	require.Equal(t, new(0), reports[0].ContextBefore)
	require.Equal(t, new(0), reports[0].ContextAfter)
	require.Nil(t, reports[1].ContextBefore)
	require.Nil(t, reports[1].ContextAfter)
	require.NotEqual(t, reports[0].CompactionID, reports[1].CompactionID)
	require.NoError(t, s.replay(t.Context(), [][]byte{
		[]byte(`{"type":"system","subtype":"compact_boundary","uuid":"historical","compact_metadata":{"trigger":"auto","pre_tokens":100}}`),
		[]byte(`{"type":"assistant","uuid":"answer","message":{"id":"reply","role":"assistant","content":[{"type":"text","text":"retained answer"}]}}`),
	}))
	require.Equal(t, reports, compactionReports(t, rec.snapshot()))
	require.Equal(t, "retained answer", agentText(rec.snapshot()))
}

type compactionFailureClient struct {
	*recorder
	failed bool
}

func (c *compactionFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if notification.Meta[wire.CompactionKey] != nil && !c.failed {
		c.failed = true

		return errors.New("compaction delivery unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestCompactionSendFailureKeepsRuntime(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := &compactionFailureClient{recorder: newRecorder()}
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt := &runtime{cancel: cancel}
	s.runtime = rt
	for _, id := range []string{"failed", "next"} {
		s.handleEvent(ctx, rt, claude.Event{Type: nativeSystem, Subtype: nativeCompactBoundary, UUID: id})
	}
	require.NoError(t, ctx.Err())
	require.True(t, rec.failed)
	require.Len(t, compactionReports(t, rec.snapshot()), 1)
}
