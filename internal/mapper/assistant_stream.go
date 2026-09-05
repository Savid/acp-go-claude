package mapper

import (
	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

// AssistantStream tracks which native assistant messages have already had their
// text and thinking delivered to the host as streamed chunks.
//
// Claude is launched with `--include-partial-messages`, so one assistant message
// reaches this mapper twice: first as `stream_event` frames (a `message_start`
// naming the API message id, then `content_block_start` and `content_block_delta`
// frames carrying the text), and then as one terminal `assistant` frame carrying
// the finished content array. Both map to assistant content, so without this
// state a host that appends every `agent_message_chunk` renders every message
// twice. The terminal frame is the one suppressed, because the streamed prefix is
// what the host already rendered live.
//
// Suppression is proven rather than assumed: only an id this stream actually
// emitted a chunk for is suppressed, so an assistant frame that arrives with no
// preceding deltas — a non-streaming reply, or transcript replay, which carries
// no `stream_event` frames at all — still emits its text exactly once.
//
// The in-flight id is kept per parent tool-use id because a subagent's frames
// interleave with the main agent's on one native stream; the emitted set is keyed
// by the API message id alone, which is unique across both.
//
// A nil *AssistantStream is a valid stream that records nothing and suppresses
// nothing: replay paths pass no stream and keep their single emission.
type AssistantStream struct {
	current map[string]string
	emitted map[string]struct{}
}

// NewAssistantStream returns the mapping state one native incarnation owns. It is
// used single-threaded under the session's foreground, like the tool-use map
// beside it.
func NewAssistantStream() *AssistantStream {
	return &AssistantStream{
		current: make(map[string]string),
		emitted: make(map[string]struct{}),
	}
}

// begin records the API message id a `message_start` opened for one agent, so a
// later chunk emitted from that agent's deltas can be attributed to it.
func (a *AssistantStream) begin(parentToolUseID string, messageID string) {
	if a == nil || messageID == "" {
		return
	}

	a.current[parentToolUseID] = messageID
}

// recordEmitted marks the in-flight message as one whose assistant content the
// host has already received. It is called only after a chunk was actually
// produced, so a `content_block_start` that opened a tool-use block — which emits
// no assistant content — marks nothing.
func (a *AssistantStream) recordEmitted(parentToolUseID string) {
	if a == nil {
		return
	}

	messageID := a.current[parentToolUseID]
	if messageID == "" {
		return
	}

	a.emitted[messageID] = struct{}{}
}

// alreadyEmitted reports whether the streamed prefix of this message id already
// reached the host. An unnamed message was never streamed under any id, so it is
// never suppressed.
func (a *AssistantStream) alreadyEmitted(messageID string) bool {
	if a == nil || messageID == "" {
		return false
	}

	_, emitted := a.emitted[messageID]

	return emitted
}

// carriesAssistantContent reports whether the mapped updates contain assistant
// text or thinking. Only those two kinds are streamed as deltas, so only they
// prove the terminal frame would repeat something.
func carriesAssistantContent(updates []acp.SessionUpdate) bool {
	for _, update := range updates {
		if update.AgentThoughtChunk != nil {
			return true
		}

		if update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil {
			return true
		}
	}

	return false
}

// streamEventMessageID reads the API message id a `message_start` event names.
// It is the `message.id` the terminal `assistant` frame repeats, and it is
// distinct from that frame's transcript `uuid`, which names the durable entry
// rather than the API message.
func streamEventMessageID(event map[string]any) string {
	message, ok := mapInput(event, keyMessage)
	if !ok {
		return ""
	}

	return stringInput(message, keyID)
}

// assistantAPIMessageID reads the API message id off a terminal `assistant`
// frame, which is the identity a `message_start` announced.
func assistantAPIMessageID(msg *claude.AssistantMessage) string {
	if msg == nil {
		return ""
	}

	message, ok := mapInput(msg.Raw, keyMessage)
	if !ok {
		return ""
	}

	return stringInput(message, keyID)
}
