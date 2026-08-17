//go:build unix

package claudeacp

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

// TestHandoffFIFOInsideTheRootDoesNotBlock drives the one non-regular file that
// can hang an open. A FIFO with no writer parks open(2) forever unless the flags
// forbid it, so the assertion is on the refusal arriving at all: the deadline is
// what fails the test instead of the suite hanging.
func TestHandoffFIFOInsideTheRootDoesNotBlock(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fifo := filepath.Join(root, "a.png")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	refused := make(chan error, 1)

	go func() {
		refused <- openHandoffPath(t, root, fifo)
	}()

	select {
	case err := <-refused:
		requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path is not a regular file")
	case <-time.After(10 * time.Second):
		t.Fatal("opening a FIFO with no writer did not return")
	}
}
