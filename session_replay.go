package claudeacp

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
)

func (s *session) replay(ctx context.Context, rows [][]byte) error {
	state := &cycleState{replay: true}

	for _, row := range rows {
		var entry struct {
			UUID            string          `json:"uuid"`
			Type            string          `json:"type"`
			ParentToolUseID string          `json:"parentToolUseID"` //nolint:tagliatelle // Native transcript spelling.
			Message         *claude.Message `json:"message"`
		}
		if err := json.Unmarshal(row, &entry); err != nil {
			return wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
		}

		if entry.Message == nil {
			continue
		}

		rowCtx := context.WithValue(ctx, parentToolKey{}, entry.ParentToolUseID)
		switch entry.Type {
		case messageRoleAssistant:
			if err := s.projectAssistant(rowCtx, state, entry.ParentToolUseID, entry.UUID, *entry.Message); err != nil {
				return wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
			}
		case messageRoleUser:
			blocks, err := entry.Message.ContentBlocks()
			if err != nil {
				return wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
			}

			for index := range blocks {
				block := &blocks[index]
				if block.Type == contentBlockTypeText {
					if err := s.emit(rowCtx, acp.UpdateUserMessageText(block.Text)); err != nil {
						return err
					}
				}

				if block.Type == contentBlockTypeImage {
					out, failure := decodeOutputImage(*block, s.agent.options.ImageLimits.core().EffectiveOutputPerImage())
					if failure != nil {
						return wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, failure)
					}

					if err := s.emit(rowCtx, acp.UpdateUserMessage(acp.ImageBlock(out.Data, out.MIME))); err != nil {
						return err
					}
				}
			}

			if err := s.projectToolResults(rowCtx, state, blocks); err != nil {
				return wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
			}
		}
	}

	return nil
}
