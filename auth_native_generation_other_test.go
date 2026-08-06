//go:build !darwin

package claudeacp

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

// A Darwin generation is a Darwin keychain construct, so off Darwin there is
// nothing to prepare and every request is refused as an unproven containment —
// whichever containment mode the agent carries.

func requireKeychainGenerationWithoutBestEffort(t *testing.T, options claude.Options) {
	t.Helper()
	_, err := options.PrepareKeychainGeneration(t.Context())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}

func requireKeychainGenerationUnderBestEffort(t *testing.T, options claude.Options) {
	t.Helper()
	_, err := options.PrepareKeychainGeneration(t.Context())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}
