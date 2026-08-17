//go:build darwin

package claudeacp

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

// requireKeychainGenerationWithoutBestEffort asserts Darwin refuses to prepare a
// keychain generation while best-effort containment is not selected.
func requireKeychainGenerationWithoutBestEffort(t *testing.T, options claude.Options) {
	t.Helper()
	_, err := options.PrepareKeychainGeneration(t.Context())
	require.ErrorContains(t, err, "best-effort containment is not selected")
}

// requireKeychainGenerationUnderBestEffort asserts Darwin prepares and releases a
// keychain generation once best-effort containment is selected.
func requireKeychainGenerationUnderBestEffort(t *testing.T, options claude.Options) {
	t.Helper()
	generation, err := options.PrepareKeychainGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Finish(true))
}
