package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/wire"
)

// outputImage is one validated emitted image: the base64 payload, the
// sniffed MIME, its decoded size, and its fingerprint.
type outputImage struct {
	data        string
	mime        string
	fingerprint string
	sizeBytes   int64
}

// decodeOutputImage validates one native image block for emission through
// the core output gate. Output is not format-allowlisted: any sniffable
// raster is emitted with its sniffed MIME.
func decodeOutputImage(block claude.ContentBlock, limit int64) (outputImage, *image.OutputError) {
	if block.Source == nil {
		return outputImage{}, &image.OutputError{Reason: image.ReasonInvalidBase64, Message: "image has no source"}
	}

	data, mime, size, failure := image.DecodeInline(block.Source.Data, limit)
	if failure != nil {
		return outputImage{}, failure
	}

	if block.Source.MediaType != "" && block.Source.MediaType != mime && image.IsImageMIME(block.Source.MediaType) {
		return outputImage{}, &image.OutputError{Reason: image.ReasonMediaTypeMismatch, Message: "declared media type does not match the image"}
	}

	digest := sha256.Sum256(data)

	return outputImage{
		data:        base64.StdEncoding.EncodeToString(data),
		mime:        mime,
		fingerprint: hex.EncodeToString(digest[:]),
		sizeBytes:   size,
	}, nil
}

// toolState is the exact-id lifecycle published for one native tool call.
type toolState struct {
	published bool
	terminal  bool
	// content is the last emitted complete content array; each later
	// content-bearing update merges onto it so no delivered item disappears
	// under ACP's whole-array replacement.
	content []toolContentItem
}

type toolContentItem struct {
	content    acp.ToolCallContent
	key        string
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

		snapshot, failure = mapToolContent(tool.content, result, s.agent.options.ImageLimits.core())
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

	s.recordToolContent(state, tool, snapshot)
	tool.terminal = true

	return nil
}

func (s *session) recordToolContent(state *cycleState, tool *toolState, snapshot []toolContentItem) {
	if len(snapshot) == 0 {
		return
	}

	tool.content = snapshot

	for _, item := range snapshot {
		if item.imageBytes > 0 {
			state.imagesEmitted = true

			return
		}
	}
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

// mapToolContent builds the next complete content snapshot from the
// previously emitted one: text and validated images, merged so an already
// delivered item never disappears, bounded per tool call.
func mapToolContent(previous []toolContentItem, blocks []claude.ContentBlock, limits image.Limits) ([]toolContentItem, *image.OutputError) {
	next := make([]toolContentItem, 0, len(blocks))

	for index := range blocks {
		block := &blocks[index]

		switch block.Type {
		case contentBlockTypeText:
			if block.Text == "" {
				continue
			}

			next = append(next, toolContentItem{content: acp.ToolContent(acp.TextBlock(block.Text)), key: "text:" + block.Text})
		case contentBlockTypeImage:
			if link := remoteImageLink(*block); link != nil {
				next = append(next, toolContentItem{content: acp.ToolContent(*link), key: "uri:" + link.ResourceLink.Uri})

				continue
			}

			output, failure := decodeOutputImage(*block, limits.EffectiveOutputPerImage())
			if failure != nil {
				return nil, failure
			}

			next = append(next, toolContentItem{
				content:    acp.ToolContent(acp.ImageBlock(output.data, output.mime)),
				key:        "image:" + output.mime + ":" + output.fingerprint,
				imageBytes: output.sizeBytes,
			})
		}
	}

	merged := mergeToolContent(previous, next)

	var total int64

	for _, item := range merged {
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

	return merged, nil
}

// mergeToolContent replaces text and retains images already delivered.
func mergeToolContent(previous []toolContentItem, next []toolContentItem) []toolContentItem {
	if len(previous) == 0 {
		return next
	}

	present := make(map[string]struct{}, len(next))
	for _, item := range next {
		present[item.key] = struct{}{}
	}

	merged := make([]toolContentItem, 0, len(previous)+len(next))

	for _, item := range previous {
		if _, ok := present[item.key]; item.imageBytes > 0 && !ok {
			merged = append(merged, item)
		}
	}

	return append(merged, next...)
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
