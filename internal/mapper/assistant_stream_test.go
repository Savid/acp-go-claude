package mapper

import (
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

// messageStartFrame is the native frame that names one API message id.
func messageStartFrame(messageID string, parentToolUseID string) *claude.StreamEventMessage {
	return &claude.StreamEventMessage{
		EventType:       streamEventMessageStart,
		ParentToolUseID: parentToolUseID,
		Event: map[string]any{
			keyType:    streamEventMessageStart,
			keyMessage: map[string]any{keyID: messageID, keyType: "message"},
		},
	}
}

// textBlockStartFrame opens a text content block, the frame that carries the
// block's initial (usually empty) text.
func textBlockStartFrame(text string, parentToolUseID string) *claude.StreamEventMessage {
	return &claude.StreamEventMessage{
		EventType:       streamEventContentBlockStart,
		ParentToolUseID: parentToolUseID,
		Event: map[string]any{
			"content_block": map[string]any{keyType: typeText, keyText: text},
		},
	}
}

func textDeltaFrame(text string, parentToolUseID string) *claude.StreamEventMessage {
	return &claude.StreamEventMessage{
		EventType:       streamEventContentBlockDelta,
		ParentToolUseID: parentToolUseID,
		Event: map[string]any{
			keyDelta: map[string]any{keyType: streamEventTextDelta, keyText: text},
		},
	}
}

func thinkingDeltaFrame(thinking string, parentToolUseID string) *claude.StreamEventMessage {
	return &claude.StreamEventMessage{
		EventType:       streamEventContentBlockDelta,
		ParentToolUseID: parentToolUseID,
		Event: map[string]any{
			keyDelta: map[string]any{keyType: streamEventThinkingDelta, keyThinking: thinking},
		},
	}
}

// terminalAssistantFrame is the finished `assistant` frame, which restates the
// whole content array and names the same API message id the opening frame did.
// `uuid` is the durable transcript identity and is deliberately distinct from it.
func terminalAssistantFrame(
	messageID string,
	parentToolUseID string,
	stopReason string,
	blocks ...map[string]any,
) *claude.AssistantMessage {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		content = append(content, block)
	}

	raw := map[string]any{
		keyType: claude.MessageTypeAssistant,
		"uuid":  "durable-" + messageID,
		keyMessage: map[string]any{
			keyID:            messageID,
			keyType:          "message",
			"stop_reason":    stopReason,
			keyContent:       content,
			"model":          "claude-sonnet-5",
			"parent_tool_id": parentToolUseID,
		},
	}
	if parentToolUseID != "" {
		raw["parent_tool_use_id"] = parentToolUseID
	}

	msg, err := claude.ParseMessage(raw)
	if err != nil {
		panic(err)
	}

	assistant, _ := msg.(*claude.AssistantMessage)

	return assistant
}

func thinkingBlock(thinking string) map[string]any {
	return map[string]any{keyType: claude.BlockTypeThinking, keyThinking: thinking}
}

func liveStreamOptions() ToolUpdateOptions {
	return ToolUpdateOptions{
		ToolUses:  map[string]claude.ToolUseBlock{},
		Assistant: NewAssistantStream(),
	}
}

func assistantTexts(updates []acp.SessionUpdate) []string {
	texts := make([]string, 0, len(updates))
	for _, update := range updates {
		if update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil {
			texts = append(texts, update.AgentMessageChunk.Content.Text.Text)
		}
	}

	return texts
}

func thoughtTexts(updates []acp.SessionUpdate) []string {
	texts := make([]string, 0, len(updates))
	for _, update := range updates {
		if update.AgentThoughtChunk != nil && update.AgentThoughtChunk.Content.Text != nil {
			texts = append(texts, update.AgentThoughtChunk.Content.Text.Text)
		}
	}

	return texts
}

// TestStreamedAssistantTextIsNotRepeatedByTerminalFrame is the regression the
// whole file exists for: one assistant message delivered by the partial-message
// stream and then restated by its terminal frame must reach the host exactly
// once. Before the suppression the terminal frame emitted the complete text a
// second time, so a host appending every chunk saw the message twice.
func TestStreamedAssistantTextIsNotRepeatedByTerminalFrame(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	require.Empty(t, MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options),
		"message_start carries no content of its own")

	streamed := assistantTexts(MessageToUpdatesWithOptions(textBlockStartFrame("", ""), options))
	streamed = append(streamed, assistantTexts(MessageToUpdatesWithOptions(textDeltaFrame("SMOKE-", ""), options))...)
	streamed = append(streamed, assistantTexts(MessageToUpdatesWithOptions(textDeltaFrame("OK", ""), options))...)
	require.Equal(t, []string{"", "SMOKE-", "OK"}, streamed)

	terminal := MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", textBlock("SMOKE-OK")), options)
	require.Empty(t, assistantTexts(terminal),
		"the terminal frame must not restate text the stream already delivered")
}

// TestTerminalAssistantFrameEmitsWithoutPrecedingDeltas proves the suppression is
// evidence-based: a message the stream never chunked is still delivered by its
// terminal frame, so a non-streaming reply survives.
func TestTerminalAssistantFrameEmitsWithoutPrecedingDeltas(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	// No message_start and no deltas at all.
	terminal := MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", textBlock("only copy"), thinkingBlock("reasoned")), options)
	require.Equal(t, []string{"only copy"}, assistantTexts(terminal))
	require.Equal(t, []string{"reasoned"}, thoughtTexts(terminal))

	// A message_start alone proves nothing was emitted either.
	other := liveStreamOptions()
	MessageToUpdatesWithOptions(messageStartFrame("msg_2", ""), other)
	require.Equal(t, []string{"still mine"}, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_2", "", "", textBlock("still mine")), other)))
}

// TestTerminalAssistantFrameForADifferentMessageStillEmits keeps suppression
// scoped to the exact API message id the stream delivered.
func TestTerminalAssistantFrameForADifferentMessageStillEmits(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options)
	MessageToUpdatesWithOptions(textDeltaFrame("first", ""), options)

	require.Empty(t, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", textBlock("first")), options)))
	require.Equal(t, []string{"second"}, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_2", "", "", textBlock("second")), options)))
}

// TestStreamedThinkingIsNotRepeatedByTerminalFrame covers the reasoning channel,
// which the stream chunks under its own delta type.
func TestStreamedThinkingIsNotRepeatedByTerminalFrame(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options)
	require.Equal(t, []string{"weighing"},
		thoughtTexts(MessageToUpdatesWithOptions(thinkingDeltaFrame("weighing", ""), options)))

	terminal := MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", thinkingBlock("weighing"), textBlock("answer")), options)
	require.Empty(t, thoughtTexts(terminal))
	require.Empty(t, assistantTexts(terminal),
		"one message's text and thinking are suppressed together, because the stream delivered both")
}

// TestSuppressedTerminalFrameStillEmitsToolCalls proves only assistant text and
// thinking are dropped. Every other block of the restated content array, and the
// terminal frame's own identity handling, is untouched.
func TestSuppressedTerminalFrameStillEmitsToolCalls(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options)
	MessageToUpdatesWithOptions(textDeltaFrame("calling a tool", ""), options)

	terminal := MessageToUpdatesWithOptions(terminalAssistantFrame("msg_1", "", "end_turn",
		textBlock("calling a tool"),
		map[string]any{
			keyType:  claude.BlockTypeToolUse,
			keyID:    "toolu_1",
			"name":   "Read",
			"input":  map[string]any{keyFilePath: absTestPath("tmp", "a")},
			"cached": false,
		},
	), options)

	require.Empty(t, assistantTexts(terminal))
	require.Len(t, terminal, 1)
	require.Equal(t, acp.ToolCallId("toolu_1"), terminal[0].ToolCall.ToolCallId)
	require.Equal(t, "durable-msg_1", assistantMessageIDOf(t, terminal[0]),
		"the terminal frame's checkpoint identity still rides the updates it does emit")
}

// TestToolUseBlockStartMarksNothing keeps the marking honest: a stream frame that
// opened a tool-use block emitted no assistant content, so the terminal frame's
// text is still owed to the host.
func TestToolUseBlockStartMarksNothing(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options)
	toolStart := MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: streamEventContentBlockStart,
		Event: map[string]any{
			"content_block": map[string]any{
				keyType: claude.BlockTypeToolUse,
				keyID:   "toolu_1",
				"name":  "Read",
				"input": map[string]any{keyFilePath: absTestPath("tmp", "a")},
			},
		},
	}, options)
	require.Len(t, toolStart, 1)
	require.Empty(t, assistantTexts(toolStart))

	require.Equal(t, []string{"never streamed"}, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", textBlock("never streamed")), options)))
}

// TestSubagentAndMainAgentStreamsDoNotCrossAttribute covers interleaving: a
// subagent's frames share the native stream with the main agent's, so the
// in-flight id is held per agent while the delivered set is keyed by the API
// message id alone.
func TestSubagentAndMainAgentStreamsDoNotCrossAttribute(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_main", ""), options)
	MessageToUpdatesWithOptions(messageStartFrame("msg_child", "toolu_parent"), options)
	MessageToUpdatesWithOptions(textDeltaFrame("child text", "toolu_parent"), options)

	// Only the subagent streamed, so only the subagent's terminal frame is
	// suppressed; the main agent's message was never chunked.
	require.Empty(t, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_child", "toolu_parent", "", textBlock("child text")), options)))
	require.Equal(t, []string{"main text"}, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_main", "", "", textBlock("main text")), options)))

	// Now the main agent streams too, under the id its own message_start named.
	MessageToUpdatesWithOptions(textDeltaFrame("main second", ""), options)
	require.Empty(t, assistantTexts(MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_main", "", "", textBlock("main second")), options)))
}

// TestNilAssistantStreamSuppressesNothing is the replay contract: transcript
// replay carries no `stream_event` frames, passes no stream, and keeps its single
// emission together with the saved assistant identity.
func TestNilAssistantStreamSuppressesNothing(t *testing.T) {
	t.Parallel()

	replay := ToolUpdateOptions{
		ToolUses:                map[string]claude.ToolUseBlock{},
		ReplayAssistantIdentity: true,
	}

	// A message_start on a stream-less options value records nothing and panics
	// on nothing.
	require.Empty(t, MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), replay))
	require.Equal(t, []string{"replayed"},
		assistantTexts(MessageToUpdatesWithOptions(textDeltaFrame("replayed", ""), replay)))

	updates := MessageToUpdatesWithOptions(
		terminalAssistantFrame("msg_1", "", "", textBlock("replayed")), replay)
	require.Equal(t, []string{"replayed"}, assistantTexts(updates))
	require.Equal(t, "durable-msg_1", assistantMessageIDOf(t, updates[0]),
		"replay keeps stamping the durable identity on the assistant update")
}

// TestAssistantStreamIdentityEdges covers the malformed and absent identities the
// native protocol can present, each of which must fail closed to "not streamed".
func TestAssistantStreamIdentityEdges(t *testing.T) {
	t.Parallel()

	var absent *AssistantStream

	absent.begin("", "msg_1")
	absent.recordEmitted("")
	require.False(t, absent.alreadyEmitted("msg_1"))

	stream := NewAssistantStream()
	require.False(t, stream.alreadyEmitted(""), "an unnamed message was never streamed")

	stream.begin("", "")
	stream.recordEmitted("")
	require.False(t, stream.alreadyEmitted(""))

	// recordEmitted with no message_start ahead of it marks nothing.
	stream.recordEmitted("toolu_unknown")
	require.False(t, stream.alreadyEmitted("msg_1"))

	require.Empty(t, streamEventMessageID(map[string]any{}), "no message object names no id")
	require.Empty(t, streamEventMessageID(map[string]any{keyMessage: "not-an-object"}))
	require.Empty(t, streamEventMessageID(map[string]any{keyMessage: map[string]any{keyID: 7}}),
		"a non-string id names no message")

	require.Empty(t, assistantAPIMessageID(nil))
	require.Empty(t, assistantAPIMessageID(&claude.AssistantMessage{}))
	require.Empty(t, assistantAPIMessageID(&claude.AssistantMessage{
		Raw: map[string]any{keyMessage: "not-an-object"},
	}))
	require.Equal(t, "msg_9", assistantAPIMessageID(&claude.AssistantMessage{
		Raw: map[string]any{keyMessage: map[string]any{keyID: "msg_9"}},
	}))

	require.False(t, carriesAssistantContent(nil))
	imageChunk := MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{claude.UnknownBlock{Type: typeImage, Raw: map[string]any{
			keySource: map[string]any{keyType: sourceBase64, keyData: "img", keyMediaType: "image/png"},
		}}},
	}, ToolUpdateOptions{})
	require.Len(t, imageChunk, 1)
	require.False(t, carriesAssistantContent(imageChunk),
		"an image chunk is not streamed as deltas and proves no repetition")
	require.True(t, carriesAssistantContent([]acp.SessionUpdate{acp.UpdateAgentThoughtText("t")}))
}

// TestUnnamedTerminalFrameIsNeverSuppressed guards the fallback: a terminal frame
// the harness sent with no API message id cannot be matched against anything the
// stream delivered, so it emits rather than silently dropping content.
func TestUnnamedTerminalFrameIsNeverSuppressed(t *testing.T) {
	t.Parallel()

	options := liveStreamOptions()

	MessageToUpdatesWithOptions(messageStartFrame("msg_1", ""), options)
	MessageToUpdatesWithOptions(textDeltaFrame("streamed", ""), options)

	require.Equal(t, []string{"unnamed"}, assistantTexts(MessageToUpdatesWithOptions(
		&claude.AssistantMessage{Content: []claude.ContentBlock{claude.TextBlock{Text: "unnamed"}}},
		options)))
}

func assistantMessageIDOf(t *testing.T, update acp.SessionUpdate) string {
	t.Helper()

	meta := updateMeta(update)
	require.NotNil(t, meta)

	claudeMeta, ok := meta[keyClaude].(map[string]any)
	require.True(t, ok, "update carries no claude metadata")

	messageID, ok := claudeMeta[keyMessageID].(string)
	require.True(t, ok, "update carries no assistant message id")

	return messageID
}

// TestAppendOnlyAssistantTextConformance is the conformance battery for the
// append-only rule. Each row replays one native fixture in native order and
// asserts the property a host relies on when it renders a turn as the in-order
// concatenation of every chunk it received: the concatenation equals the final
// text exactly once, with no chunk repeating text an earlier chunk carried.
func TestAppendOnlyAssistantTextConformance(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		frames []claude.Message
		// chunks is the exact chunk sequence the fixture must produce.
		chunks []string
		// final is the assembled text a host renders from those chunks.
		final string
	}{
		{
			// The terminal frame restates the whole assembled text. The deltas
			// carried all of it, so the terminal frame contributes nothing.
			name: "terminal frame repeats the streamed text",
			frames: []claude.Message{
				messageStartFrame("msg_1", ""),
				textDeltaFrame("append-", ""),
				textDeltaFrame("only", ""),
				terminalAssistantFrame("msg_1", "", "end_turn", textBlock("append-only")),
			},
			chunks: []string{"append-", "only"},
			final:  "append-only",
		},
		{
			// A harness that streams no deltas at all yields exactly one chunk.
			name: "deltas-free turn yields exactly one chunk",
			frames: []claude.Message{
				terminalAssistantFrame("msg_1", "", "end_turn", textBlock("one copy only")),
			},
			chunks: []string{"one copy only"},
			final:  "one copy only",
		},
		{
			// Several native assistant messages in one turn: each is delivered
			// once, in native order, deduplicated on its native identity.
			name: "multi-message turn yields each message once",
			frames: []claude.Message{
				messageStartFrame("msg_1", ""),
				textDeltaFrame("first ", ""),
				terminalAssistantFrame("msg_1", "", "", textBlock("first ")),
				messageStartFrame("msg_2", ""),
				textDeltaFrame("second", ""),
				terminalAssistantFrame("msg_2", "", "end_turn", textBlock("second")),
			},
			chunks: []string{"first ", "second"},
			final:  "first second",
		},
		{
			// A terminal frame that carries more than the deltas delivered
			// contributes nothing twice: the repeated identity is suppressed as a
			// whole, so no already-streamed text is ever re-sent.
			name: "a repeated native identity is deduplicated on identity",
			frames: []claude.Message{
				messageStartFrame("msg_1", ""),
				textDeltaFrame("kept", ""),
				terminalAssistantFrame("msg_1", "", "", textBlock("kept")),
				terminalAssistantFrame("msg_1", "", "end_turn", textBlock("kept")),
			},
			chunks: []string{"kept"},
			final:  "kept",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			options := liveStreamOptions()

			chunks := make([]string, 0, len(tc.chunks))
			for _, frame := range tc.frames {
				chunks = append(chunks, assistantTexts(MessageToUpdatesWithOptions(frame, options))...)
			}

			require.Equal(t, tc.chunks, chunks)
			require.Equal(t, tc.final, strings.Join(chunks, ""),
				"the in-order concatenation of every chunk is the turn's assistant text exactly once")
			require.Equal(t, 1, strings.Count(strings.Join(chunks, ""), tc.chunks[len(tc.chunks)-1]),
				"no chunk repeats text an earlier chunk already carried")
		})
	}
}
