package claudeacp

import (
	"encoding/json"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/image"
	"github.com/stretchr/testify/require"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestAssistantImageOrderIdentityAndReplay(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	rec := newRecorder()
	agent.attach(rec, nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	session.id = "image-session"
	blocks := []claude.ContentBlock{{Type: "text", Text: "before"}, {Type: "image", Source: &claude.Source{Type: nativeBase64, MediaType: "image/png", Data: tinyPNG}}, {Type: "text", Text: "after"}}
	blocks = append(blocks[:2], append([]claude.ContentBlock{blocks[1]}, blocks[2:]...)...)
	data, err := json.Marshal(blocks)
	require.NoError(t, err)
	native := claude.Message{ID: "message-image", Role: "assistant", Content: data}
	event := claude.Event{Type: "assistant", UUID: "image-row", ParentToolUseID: "parent-tool", Message: &native}
	c := &cycle{}
	for range 2 {
		_, err = session.projectEvent(t.Context(), nil, c, event)
		require.NoError(t, err)
	}
	live := rec.snapshot()
	require.Len(t, live, 3)
	require.Equal(t, "before", live[0].Update.AgentMessageChunk.Content.Text.Text)
	require.Equal(t, tinyPNG, live[1].Update.AgentMessageChunk.Content.Image.Data)
	require.Equal(t, "after", live[2].Update.AgentMessageChunk.Content.Text.Text)
	for _, update := range live {
		require.Equal(t, "message-image", *update.Update.AgentMessageChunk.MessageId)
		meta, ok := update.Update.AgentMessageChunk.Meta[vendor].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "parent-tool", meta["parentToolUseId"])
	}
	row, err := json.Marshal(map[string]any{"type": "assistant", "uuid": "image-row", "parentToolUseID": "parent-tool", "message": native})
	require.NoError(t, err)
	require.NoError(t, session.replay(t.Context(), [][]byte{row}))
	require.Equal(t, live, rec.snapshot()[3:])
}

func TestAssistantOutputRefusalAndRemoteLink(t *testing.T) {
	t.Parallel()
	for _, source := range []claude.Source{{Type: nativeBase64, Data: tinyPNG, MediaType: "image/png"}, {Type: "url", URL: "https://example.test/image.png"}} {
		agent := NewAgent(WithImageLimits(ImageLimits{MaxOutputBytesPerImage: 10}))
		rec := newRecorder()
		agent.attach(rec, nil)
		session := agent.newSession(sessionStart{cwd: t.TempDir()})
		content, err := json.Marshal([]claude.ContentBlock{{Type: "image", Source: &source}})
		require.NoError(t, err)
		require.NoError(t, session.projectAssistant(t.Context(), &cycleState{}, "", "image-row", claude.Message{ID: "image", Content: content}))
		updates := rec.snapshot()
		require.Len(t, updates, 1)
		chunk := updates[0].Update.AgentMessageChunk.Content
		if source.Data != "" {
			require.Equal(t, image.GuidanceTooLarge, chunk.Text.Text)
		} else {
			require.Equal(t, source.URL, chunk.ResourceLink.Uri)
		}
	}
}

func TestToolOutputSnapshotsAndRawRedaction(t *testing.T) {
	t.Parallel()
	limits := image.DefaultLimits()
	imageBlock := claude.ContentBlock{Type: "image", Source: &claude.Source{Type: nativeBase64, Data: tinyPNG, MediaType: "image/png"}}
	first, failure := mapToolContent(nil, []claude.ContentBlock{imageBlock, imageBlock}, limits)
	require.Nil(t, failure)
	require.Len(t, first, 1)
	second, failure := mapToolContent(first, []claude.ContentBlock{imageBlock, {Type: "text", Text: "final"}}, limits)
	require.Nil(t, failure)
	require.Len(t, second, 2)
	require.Equal(t, tinyPNG, second[0].content.Content.Content.Image.Data)
	require.Equal(t, "final", second[1].content.Content.Content.Text.Text)
	limits.MaxOutputBytesPerToolCall = 1
	_, failure = mapToolContent(nil, []claude.ContentBlock{imageBlock}, limits)
	require.Equal(t, image.ReasonTooLarge, failure.Reason)
	raw, err := json.Marshal(map[string]any{"content": []claude.ContentBlock{imageBlock, {Type: "image", Source: &claude.Source{Type: "url", URL: "https://example.test/image?secret=value"}}}})
	require.NoError(t, err)
	var value any
	require.NoError(t, json.Unmarshal(raw, &value))
	redactImages(value)
	redacted, err := json.Marshal(value)
	require.NoError(t, err)
	require.NotContains(t, string(redacted), tinyPNG)
	require.NotContains(t, string(redacted), "secret=value")
	require.Contains(t, string(redacted), "sizeBytes")
}
