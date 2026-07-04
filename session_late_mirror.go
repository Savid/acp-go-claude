package claudeacp

import (
	"context"

	"github.com/savid/acp-go-claude/internal/mapper"
)

func (s *agentSession) startLateMirrorProcessor(ctx context.Context, options mapper.ToolUpdateOptions) {
	_ = s
	_ = ctx
	_ = options
}

func (s *agentSession) stopLateMirrorProcessor(ctx context.Context) {
	_ = s
	_ = ctx
}
