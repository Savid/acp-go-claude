package claudeacp

import (
	"context"
	"net/url"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/wire"
)

// decodeOutputImage validates one native image block for emission through the
// core output gate.
func decodeOutputImage(block claude.ContentBlock, limit int64) (image.Output, *image.OutputError) {
	if block.Source == nil {
		return image.Output{}, &image.OutputError{Reason: image.ReasonInvalidBase64, Message: "image has no source"}
	}

	return image.DecodeOutput(block.Source.Data, block.Source.MediaType, limit)
}

// toolState is the exact-id lifecycle published for one native tool call.
// Its content is published once, with the terminal status, and not retained.
type toolState struct {
	published bool
	terminal  bool
}

type toolContentItem struct {
	content    acp.ToolCallContent
	imageBytes int64
}

func (state *cycleState) tool(id string) *toolState {
	if state.tools == nil {
		state.tools = make(map[string]*toolState)
	}

	tool := state.tools[id]
	if tool == nil {
		tool = &toolState{}
		state.tools[id] = tool
	}

	return tool
}

// publishPendingTool announces a tool call that is awaiting permission before
// claude reports its execution.
func (s *session) publishPendingTool(ctx context.Context, state *cycleState, prompt claude.ControlRequest) error {
	tool := state.tool(prompt.ToolUseID)
	if tool.published {
		return nil
	}

	opts := []acp.ToolCallStartOpt{
		acp.WithStartKind(toolKindForName(prompt.ToolName)),
		acp.WithStartStatus(acp.ToolCallStatusPending),
	}
	if len(prompt.Input) > 0 {
		opts = append(opts, acp.WithStartRawInput(prompt.Input))
	}

	if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(prompt.ToolUseID), prompt.ToolName, opts...)); err != nil {
		return err
	}

	tool.published = true

	return nil
}

func (s *session) publishToolStart(ctx context.Context, state *cycleState, event claude.ContentBlock) error {
	tool := state.tool(event.ID)
	if tool.terminal {
		return nil
	}

	kind := toolKindForName(event.Name)

	if tool.published {
		opts := []acp.ToolCallUpdateOpt{
			acp.WithUpdateTitle(event.Name),
			acp.WithUpdateKind(kind),
			acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
		}
		if len(event.Input) > 0 {
			opts = append(opts, acp.WithUpdateRawInput(event.Input))
		}

		return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(event.ID), opts...))
	}

	opts := []acp.ToolCallStartOpt{acp.WithStartKind(kind), acp.WithStartStatus(acp.ToolCallStatusInProgress)}
	if len(event.Input) > 0 {
		opts = append(opts, acp.WithStartRawInput(event.Input))
	}

	if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(event.ID), event.Name, opts...)); err != nil {
		return err
	}

	tool.published = true

	return nil
}

// publishToolTerminal emits the terminal status and, when the result carries
// mappable content, the complete final snapshot.
func (s *session) publishToolTerminal(ctx context.Context, state *cycleState, toolCallID string, status acp.ToolCallStatus, result []claude.ContentBlock) error {
	tool := state.tool(toolCallID)
	if tool.terminal {
		return nil
	}

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}

	var snapshot []toolContentItem

	if result != nil {
		var failure *image.OutputError

		snapshot, failure = mapToolContent(result, s.agent.options.ImageLimits.core())
		if failure != nil {
			if state.replay {
				return failure
			}

			return s.failToolImage(ctx, state, toolCallID, failure)
		}

		if len(snapshot) > 0 {
			opts = append(opts, acp.WithUpdateContent(toolContent(snapshot)))
		}
	}

	if err := s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(toolCallID), opts...)); err != nil {
		return err
	}

	for _, item := range snapshot {
		if item.imageBytes > 0 {
			state.imagesEmitted = true
		}
	}

	tool.terminal = true

	return nil
}

// failToolImage handles an image the adapter will not ship on tool
// provenance. A verdict the model can act on carries its guidance as that
// call's own content and the turn continues; a storage failure ends the turn.
func (s *session) failToolImage(ctx context.Context, state *cycleState, toolCallID string, failure *image.OutputError) error {
	tool := state.tool(toolCallID)
	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(acp.ToolCallStatusFailed)}

	guidance, recoverable := failure.Guidance()
	if recoverable {
		opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(guidance))}))
	}

	err := s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(toolCallID), opts...))
	tool.terminal = true

	if recoverable {
		return err
	}

	return wire.TurnFailed(vendor, failure.TurnFailure())
}

// mapToolContent builds a tool call's complete content snapshot: text and
// validated images, bounded per tool call.
func mapToolContent(blocks []claude.ContentBlock, limits image.Limits) ([]toolContentItem, *image.OutputError) {
	next := make([]toolContentItem, 0, len(blocks))
	seen := make(map[string]struct{})

	for index := range blocks {
		block := &blocks[index]

		switch block.Type {
		case contentBlockTypeText:
			if block.Text == "" {
				continue
			}

			next = append(next, toolContentItem{content: acp.ToolContent(acp.TextBlock(block.Text))})
		case contentBlockTypeImage:
			if link := remoteImageLink(*block); link != nil {
				key := "uri:" + link.ResourceLink.Uri
				if _, exists := seen[key]; exists {
					continue
				}

				seen[key] = struct{}{}

				next = append(next, toolContentItem{content: acp.ToolContent(*link)})

				continue
			}

			output, failure := decodeOutputImage(*block, limits.EffectiveOutputPerImage())
			if failure != nil {
				return nil, failure
			}

			key := "image:" + output.MIME + ":" + output.Fingerprint
			if _, exists := seen[key]; exists {
				continue
			}

			seen[key] = struct{}{}

			next = append(next, toolContentItem{
				content:    acp.ToolContent(acp.ImageBlock(output.Data, output.MIME)),
				imageBytes: output.SizeBytes,
			})
		}
	}

	var total int64

	for _, item := range next {
		total += item.imageBytes
		if item.imageBytes > 0 && total > limits.EffectiveOutputPerToolCall() {
			return nil, &image.OutputError{
				Reason:    image.ReasonTooLarge,
				Message:   "tool call image content exceeds the per-tool-call limit",
				SizeBytes: total,
				MaxBytes:  limits.EffectiveOutputPerToolCall(),
			}
		}
	}

	return next, nil
}

func toolContent(items []toolContentItem) []acp.ToolCallContent {
	content := make([]acp.ToolCallContent, 0, len(items))
	for _, item := range items {
		content = append(content, item.content)
	}

	return content
}

func remoteImageLink(block claude.ContentBlock) *acp.ContentBlock {
	if block.Source == nil || block.Source.Data != "" || block.Source.URL == "" {
		return nil
	}

	uri, err := url.Parse(block.Source.URL)
	if err != nil || uri.Host == "" || (uri.Scheme != "https" && uri.Scheme != "http") {
		return nil
	}

	content := acp.ResourceLinkBlock("Image", block.Source.URL)

	return &content
}
