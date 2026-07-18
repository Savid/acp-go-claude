package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/savid/acp-go-claude/internal/claude"
)

var (
	mkdirDarwinGeneration  = os.MkdirTemp
	removeDarwinGeneration = os.RemoveAll
	chmodDarwinGeneration  = os.Chmod
)

func (a *Agent) prepareDarwinGeneration(ctx context.Context, kind RuntimeResourceKind) (*claude.DarwinGeneration, error) {
	if a == nil {
		return nil, errors.New("prepare Darwin generation: agent is unavailable")
	}

	if a.containmentMode != RuntimeContainmentBestEffort {
		return nil, errors.New("prepare Darwin generation: best-effort containment is not selected")
	}

	release, err := reserveScratchRoot(ctx, a.options.RuntimeResourceHooks, kind)
	if err != nil {
		return nil, err
	}

	parent, err := ensureScratchParent(a.options.ScratchDir)
	if err != nil {
		release()

		return nil, err
	}

	root, err := mkdirDarwinGeneration(parent, "acp-go-claude-runtime-")
	if err != nil {
		release()

		return nil, fmt.Errorf("create Darwin runtime generation: %w", err)
	}

	if chmodErr := chmodDarwinGeneration(root, 0o700); chmodErr != nil {
		_ = removeDarwinGeneration(root)

		release()

		return nil, fmt.Errorf("secure Darwin runtime generation: %w", chmodErr)
	}

	generation, err := claude.NewDarwinGenerationRecord(parent, root, string(kind))
	if err != nil {
		removeErr := removeDarwinGeneration(root)
		if removeErr == nil {
			release()
		}

		return nil, errors.Join(err, removeErr)
	}

	generation.Release = func(complete bool) error {
		if !complete {
			return nil
		}

		removeErr := removeDarwinGeneration(root)
		if removeErr == nil {
			release()
		}

		return removeErr
	}

	return generation, nil
}

func (a *Agent) prepareUsageGeneration(ctx context.Context) (*claude.DarwinGeneration, error) {
	if a == nil {
		return nil, errors.New("prepare usage generation: agent is unavailable")
	}

	if a.containmentMode == RuntimeContainmentBestEffort {
		return a.prepareDarwinGeneration(ctx, RuntimeResourceDiscovery)
	}

	if a.containmentMode != RuntimeContainmentAuthoritative {
		return nil, ErrProcessContainmentIncomplete
	}

	release, err := reserveScratchRoot(ctx, a.options.RuntimeResourceHooks, RuntimeResourceDiscovery)
	if err != nil {
		return nil, err
	}

	parent, err := ensureScratchParent(a.options.ScratchDir)
	if err != nil {
		release()

		return nil, err
	}

	root, err := mkdirDarwinGeneration(parent, "acp-go-claude-runtime-")
	if err != nil {
		release()

		return nil, fmt.Errorf("create usage runtime generation: %w", err)
	}

	if chmodErr := chmodDarwinGeneration(root, 0o700); chmodErr != nil {
		_ = removeDarwinGeneration(root)

		release()

		return nil, fmt.Errorf("secure usage runtime generation: %w", chmodErr)
	}

	return &claude.DarwinGeneration{
		ScratchRoot: root,
		Release: func(complete bool) error {
			if !complete {
				return nil
			}

			removeErr := removeDarwinGeneration(root)
			if removeErr == nil {
				release()
			}

			return removeErr
		},
	}, nil
}
