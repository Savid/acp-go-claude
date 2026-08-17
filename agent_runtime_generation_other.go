//go:build !darwin

package claudeacp

import (
	"context"

	"github.com/savid/acp-go-claude/internal/claude"
)

func (*Agent) prepareDarwinGeneration(context.Context, RuntimeResourceKind) (*claude.DarwinGeneration, error) {
	return nil, ErrProcessContainmentIncomplete
}
