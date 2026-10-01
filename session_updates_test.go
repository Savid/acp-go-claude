package claudeacp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestReplayKeepsFragmentsWithOneAPIMessageID(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	rec := newRecorder()
	agent.attach(rec, nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	session.id = "replay-session"
	rows := make([][]byte, 0, 3)
	rows = append(rows,
		[]byte(`{"type":"assistant","uuid":"thought","message":{"id":"api-message","role":"assistant","content":[{"type":"thinking","thinking":"considering"}]}}`),
		[]byte(`{"type":"assistant","uuid":"text","message":{"id":"api-message","role":"assistant","content":[{"type":"text","text":"answer"}]}}`),
	)
	rows = append(rows, rows[1])
	require.NoError(t, session.replay(t.Context(), rows))
	require.Equal(t, "answer", agentText(rec.snapshot()))
}
func TestPromptContentOrderAndDocuments(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	mime := "application/pdf"
	blocks := []acp.ContentBlock{acp.TextBlock("before"), {Resource: &acp.ContentBlockResource{Resource: acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{MimeType: &mime, Blob: base64.StdEncoding.EncodeToString([]byte("%PDF-"))}}}}, acp.TextBlock("after")}
	prompt, err := session.mapPrompt(t.Context(), blocks)
	require.NoError(t, err)
	require.Len(t, prompt.content, 3)
	require.Equal(t, "before", prompt.content[0].Text)
	require.Equal(t, "document", prompt.content[1].Type)
	require.Equal(t, mime, prompt.content[1].Source.MediaType)
	require.Equal(t, "after", prompt.content[2].Text)
}

func TestReplayRejectsInvalidImageBytes(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	agent.attach(newRecorder(), nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	session.id = "replay-session"
	rows := [][]byte{[]byte(`{"type":"assistant","uuid":"image","message":{"id":"api-image","role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%%%"}}]}}`)}
	err := session.replay(t.Context(), rows)
	require.Equal(t, "claude_restore_failed", requestErrorData(t, err)["error"])
}

func TestCommandCatalogFollowsRuntime(t *testing.T) {
	t.Parallel()
	for _, commands := range []string{"native", "empty"} {
		t.Run(commands, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", "ACP_GO_CLAUDE_TEST_COMMANDS": commands}))
			h.initialize()
			session := h.newSession()
			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(commandSnapshots(updates)) > 0 })
			initial := commandSnapshots(h.rec.snapshot())[0]
			if commands == "empty" {
				require.Empty(t, initial)
			} else {
				require.Len(t, initial, 1)
				require.Equal(t, "compact", initial[0].Name)
			}
			_, err := h.prompt(session.SessionId, "CRASH", nil)
			require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
			_, err = h.prompt(session.SessionId, "HELLO", nil)
			require.NoError(t, err)
			catalogs := commandSnapshots(h.rec.snapshot())
			require.Len(t, catalogs, 2)
			require.Equal(t, initial, catalogs[len(catalogs)-1])
		})
	}
}
func commandSnapshots(updates []acp.SessionNotification) [][]acp.AvailableCommand {
	var snapshots [][]acp.AvailableCommand
	for _, update := range updates {
		if commands := update.Update.AvailableCommandsUpdate; commands != nil {
			snapshots = append(snapshots, commands.AvailableCommands)
		}
	}

	return snapshots
}

// TestUsageFollowsEachResponse proves every model call of a turn reports the
// context its request occupies when it starts and the context it leaves once
// its output is known, never the running sum, while the prompt response
// carries the turn's summed consumption.
func TestUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "MULTI", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 1100},
		{Size: 1000, Used: 1120},
		{Size: 1000, Used: 1170},
		{Size: 1000, Used: 1200},
		{Size: 1000, Used: 1240},
		{Size: 1000, Used: 1250},
		{Size: 1000, Used: 1250, Cost: usageCost(3)},
	}, usageUpdates(h.rec.snapshot()))
	require.NotNil(t, resp.Usage)
	require.Equal(t, 3570, resp.Usage.TotalTokens)
	require.Equal(t, 190, resp.Usage.InputTokens)
	require.Equal(t, 60, resp.Usage.OutputTokens)
}

// TestUsageReportsResponseInputBeforeOutput proves a call reports the context
// its request occupies before any of its output streams, and adds the output
// once the call has finished streaming it.
func TestUsageReportsResponseInputBeforeOutput(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	var order []string

	for _, update := range h.rec.snapshot() {
		switch {
		case update.Update.UsageUpdate != nil:
			order = append(order, fmt.Sprintf("usage:%d", update.Update.UsageUpdate.Used))
		case update.Update.AgentMessageChunk != nil:
			order = append(order, "text")
		}
	}

	require.Equal(t, []string{"usage:10", "text", "text", "usage:15", "usage:15"}, order)
}

// TestUnusableResponsesReportNoUsage proves a call whose provider reports no
// usage sends nothing, and an attempt the provider dropped mid-stream reports
// only the request it sent while the retried call reports in full.
func TestUnusableResponsesReportNoUsage(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prompt string
		want   []acp.SessionUsageUpdate
	}{
		"no usage reported": {"EMPTY", nil},
		"dropped attempt retried": {"RETRY", []acp.SessionUsageUpdate{
			{Size: 1000, Used: 1100},
			{Size: 1000, Used: 1100},
			{Size: 1000, Used: 1120},
			{Size: 1000, Used: 1120, Cost: usageCost(1)},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.initialize()
			session := h.newSession()

			_, err := h.prompt(session.SessionId, tc.prompt, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, usageUpdates(h.rec.snapshot()))
		})
	}
}

// TestUsageResponseShapes proves calls a provider reports only at the end of
// the stream, calls claude made without streaming, and calls after input
// queued mid-run each report their own context.
func TestUsageResponseShapes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prompt string
		want   []acp.SessionUsageUpdate
	}{
		"usage only at stream end": {"GATEWAY", []acp.SessionUsageUpdate{
			{Size: 1000, Used: 1040},
			{Size: 1000, Used: 1040, Cost: usageCost(1)},
		}},
		"no stream": {"NOSTREAM", []acp.SessionUsageUpdate{
			{Size: 1000, Used: 500},
			{Size: 1000, Used: 500, Cost: usageCost(1)},
		}},
		"input queued mid-run": {"STEER", []acp.SessionUsageUpdate{
			{Size: 1000, Used: 1100},
			{Size: 1000, Used: 1120},
			{Size: 1000, Used: 1150},
			{Size: 1000, Used: 1160},
			{Size: 1000, Used: 1160, Cost: usageCost(2)},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.initialize()
			session := h.newSession()

			_, err := h.prompt(session.SessionId, tc.prompt, nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, usageUpdates(h.rec.snapshot()))
		})
	}
}

// TestSettledUsageAfterCompaction proves no figure restates the context a
// compaction replaced: the next call reports the compacted context, and a turn
// with no call after its compaction settles without usage.
func TestSettledUsageAfterCompaction(t *testing.T) {
	t.Parallel()

	t.Run("call after compaction", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.initialize()
		session := h.newSession()

		_, err := h.prompt(session.SessionId, "COMPACT", nil)
		require.NoError(t, err)
		require.Equal(t, []acp.SessionUsageUpdate{
			{Size: 1000, Used: 900},
			{Size: 1000, Used: 950},
			{Size: 1000, Used: 310},
			{Size: 1000, Used: 315},
			{Size: 1000, Used: 315, Cost: usageCost(2)},
		}, usageUpdates(h.rec.snapshot()))
	})

	t.Run("no call after compaction", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.initialize()
		session := h.newSession()

		_, err := h.prompt(session.SessionId, "COMPACTEND", nil)
		require.NoError(t, err)
		require.Equal(t, []acp.SessionUsageUpdate{{Size: 1000, Used: 900}, {Size: 1000, Used: 950}}, usageUpdates(h.rec.snapshot()))

		_, err = h.prompt(session.SessionId, "HELLO", nil)
		require.NoError(t, err)
		require.Equal(t, []acp.SessionUsageUpdate{
			{Size: 1000, Used: 900},
			{Size: 1000, Used: 950},
			{Size: 1000, Used: 10},
			{Size: 1000, Used: 15},
			{Size: 1000, Used: 15, Cost: usageCost(2)},
		}, usageUpdates(h.rec.snapshot()))
	})
}

// TestUsageSizeFollowsSelectedModel proves the context window is known from
// session start, before any result, and follows a model change; the result's
// entry for another model never sizes the session.
func TestUsageSizeFollowsSelectedModel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "haiku"))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 10},
		{Size: 1000, Used: 15},
		{Size: 1000, Used: 15, Cost: usageCost(1)},
		{Size: 500, Used: 10},
		{Size: 500, Used: 15},
		{Size: 500, Used: 15, Cost: usageCost(2)},
	}, usageUpdates(h.rec.snapshot()))
}

func TestCancelledTurnReportsNoUsageAfterCancel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	done := make(chan acp.PromptResponse, 1)

	go func() {
		resp, _ := h.prompt(session.SessionId, "STEPSLOW", nil)
		done <- resp
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates)) == 3 })
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 1100},
		{Size: 1000, Used: 1120},
		{Size: 1000, Used: 1170},
	}, usageUpdates(h.rec.snapshot()))
}

func TestAgentOriginUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "AGENTWORK", promptMeta(1))
	require.NoError(t, err)

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return idleTransitions(updates, session.SessionId) == 2 })

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 10},
		{Size: 1000, Used: 15},
		{Size: 1000, Used: 15, Cost: usageCost(1)},
		{Size: 1000, Used: 10},
		{Size: 1000, Used: 15},
		{Size: 1000, Used: 15, Cost: usageCost(2)},
	}, usageUpdates(h.rec.snapshot()))
}

func TestCancelledAgentOriginCycleReportsNoUsageAfterCancel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "AGENTHANG", promptMeta(1))
	require.NoError(t, err)

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates)) == 4 })
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return idleTransitions(updates, session.SessionId) == 2 })

	events := lifecycleEvents(h.rec.snapshot())
	require.Equal(t, "cancelled", events[len(events)-1]["outcome"])
	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: 1000, Used: 10},
		{Size: 1000, Used: 15},
		{Size: 1000, Used: 15, Cost: usageCost(1)},
		{Size: 1000, Used: 10},
	}, usageUpdates(h.rec.snapshot()))
}

// TestCapturedCallUsageShapes replays one captured model call per provider
// shape under testdata/native: each reports its request when the provider
// states it at message_start, and the context it leaves at message_delta.
func TestCapturedCallUsageShapes(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/native/call-usage.json")
	require.NoError(t, err)

	var shapes map[string][]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &shapes))

	for name, want := range map[string][]acp.SessionUsageUpdate{
		"anthropic":  {{Size: 200000, Used: 21714}, {Size: 200000, Used: 21908}},
		"gateway":    {{Size: 200000, Used: 20298}},
		"openrouter": {{Size: 200000, Used: 20202}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			rec := newRecorder()
			agent.attach(rec, nil)
			session := agent.newSession(sessionStart{cwd: t.TempDir()})
			session.contextWindow = 200000
			c := &cycle{}

			require.NotEmpty(t, shapes[name])

			for _, frame := range shapes[name] {
				var event claude.Event
				require.NoError(t, json.Unmarshal(frame, &event))

				_, err := session.projectEvent(t.Context(), nil, c, event)
				require.NoError(t, err)
			}

			require.Equal(t, want, usageUpdates(rec.snapshot()))
		})
	}
}

// TestLiveAssistantStateHoldsOneMessage proves a live stream keeps only the
// current API message's rows for deduplication, so a long turn does not
// accumulate them, while a repeated row of that message still emits once.
func TestLiveAssistantStateHoldsOneMessage(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	rec := newRecorder()
	agent.attach(rec, nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	state := &cycleState{}

	for _, id := range []string{"one", "two", "three"} {
		for _, row := range []string{"a", "b", "b"} {
			message := claude.Message{ID: id, Role: messageRoleAssistant, Content: json.RawMessage(`[{"type":"text","text":"` + id + row + `"}]`)}
			require.NoError(t, session.projectAssistant(t.Context(), state, "", id+row, message))
		}
	}

	require.Equal(t, "oneaonebtwoatwobthreeathreeb", agentText(rec.snapshot()))
	require.Len(t, state.message("").finalized, 2)
}
