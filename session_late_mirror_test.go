package claudeacp

import (
	"context"
	"testing"

	"github.com/savid/acp-go-claude/internal/mapper"
)

func TestLateMirrorNoopBranches(t *testing.T) {
	t.Parallel()

	session := &agentSession{}
	session.startLateMirrorProcessor(context.Background(), mapper.ToolUpdateOptions{})
	session.stopLateMirrorProcessor(context.Background())
}
