package claudeacp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func requireHandoffRefusal(t *testing.T, err error, verdict string, message string) {
	t.Helper()

	var refused *mapper.HandoffPathError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, verdict, refused.Verdict)
	require.Equal(t, message, refused.Message)
	require.Equal(t, message, refused.Error())
}

func TestValidateInputHandoffRoot(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateInputHandoffRoot(""))
	require.NoError(t, validateInputHandoffRoot(filepath.Join(string(filepath.Separator), "srv", "handoff")))
	require.EqualError(t, validateInputHandoffRoot("handoff"), "InputHandoffRoot must be an absolute path")
}

func TestNewHandoffImageReaderRequiresARoot(t *testing.T) {
	t.Parallel()

	require.Nil(t, newHandoffImageReader(""))
	require.NotNil(t, newHandoffImageReader(t.TempDir()))
}

func TestHandoffImageReaderReadsInsideTheRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "a.png")
	payload := []byte("handoff-image-bytes")
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	reader := newHandoffImageReader(root)

	// A subpath of any depth is readable.
	nested := filepath.Join(root, "session", "operation")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	nestedPath := filepath.Join(nested, "b.png")
	require.NoError(t, os.WriteFile(nestedPath, payload, 0o600))

	for _, target := range []string{path, nestedPath} {
		file, err := reader.ReadHandoffImage(context.Background(), target, int64(len(payload)))
		require.NoError(t, err)
		require.Equal(t, payload, file.Data)
		require.EqualValues(t, len(payload), file.Size)
		require.False(t, file.Truncated)
	}

	// A file past the gate is read one byte over it and reported truncated with
	// its real size.
	file, err := reader.ReadHandoffImage(context.Background(), path, int64(len(payload))-5)
	require.NoError(t, err)
	require.Len(t, file.Data, len(payload)-4)
	require.EqualValues(t, len(payload), file.Size)
	require.True(t, file.Truncated)

	// No gate means no bound.
	file, err = reader.ReadHandoffImage(context.Background(), path, 0)
	require.NoError(t, err)
	require.Equal(t, payload, file.Data)
	require.False(t, file.Truncated)
}

func TestHandoffImageReaderRefusals(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	reader := newHandoffImageReader(root)

	outsideFile := filepath.Join(outside, "a.png")
	require.NoError(t, os.WriteFile(outsideFile, []byte("bytes"), 0o600))

	_, err := reader.ReadHandoffImage(context.Background(), outsideFile, 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path is outside the configured handoff root")

	// A traversal that lands outside is refused on the cleaned path.
	_, err = reader.ReadHandoffImage(context.Background(), filepath.Join(root, "..", "a.png"), 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path is outside the configured handoff root")

	// A symlink inside the root that points out of it is refused after
	// resolution, not before.
	escape := filepath.Join(root, "escape.png")
	require.NoError(t, os.Symlink(outsideFile, escape))
	_, err = reader.ReadHandoffImage(context.Background(), escape, 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path escapes the configured handoff root")

	// A path inside the root that does not exist is the host's cleanup race.
	_, err = reader.ReadHandoffImage(context.Background(), filepath.Join(root, "missing.png"), 0)
	requireHandoffRefusal(t, err, mapper.HandoffMissingFile, "handoff file does not exist")

	// A directory is not a regular file.
	directory := filepath.Join(root, "dir")
	require.NoError(t, os.Mkdir(directory, 0o700))
	_, err = reader.ReadHandoffImage(context.Background(), directory, 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path is not a regular file")

	// A cancelled turn surfaces as itself rather than as a path verdict.
	path := filepath.Join(root, "a.png")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reader.ReadHandoffImage(cancelled, path, 0)
	require.ErrorIs(t, err, context.Canceled)
}

func TestHandoffImageReaderFilesystemFailures(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.png")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))
	reader := newHandoffImageReader(root)
	failure := errors.New("filesystem failure")

	originalEval := handoffEvalSymlinks
	originalStat := handoffStat
	originalOpen := handoffOpen
	originalReadAll := handoffReadAll

	t.Cleanup(func() {
		handoffEvalSymlinks = originalEval
		handoffStat = originalStat
		handoffOpen = originalOpen
		handoffReadAll = originalReadAll
	})

	handoffEvalSymlinks = func(string) (string, error) { return "", failure }

	_, err := reader.ReadHandoffImage(context.Background(), path, 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff path cannot be resolved safely")

	handoffEvalSymlinks = originalEval

	handoffStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	_, err = reader.ReadHandoffImage(context.Background(), path, 0)
	requireHandoffRefusal(t, err, mapper.HandoffMissingFile, "handoff file does not exist")

	handoffStat = func(string) (os.FileInfo, error) { return nil, failure }

	_, err = reader.ReadHandoffImage(context.Background(), path, 0)
	requireHandoffRefusal(t, err, mapper.HandoffPathNotAllowed, "handoff file cannot be inspected")

	handoffStat = originalStat

	handoffOpen = func(string) (*os.File, error) { return nil, failure }

	_, err = reader.ReadHandoffImage(context.Background(), path, 0)
	requireHandoffRefusal(t, err, mapper.HandoffMissingFile, "handoff file cannot be opened")

	handoffOpen = originalOpen

	handoffReadAll = func(io.Reader) ([]byte, error) { return nil, failure }

	_, err = reader.ReadHandoffImage(context.Background(), path, 0)
	requireHandoffRefusal(t, err, mapper.HandoffMissingFile, "handoff file cannot be read")

	handoffReadAll = originalReadAll
}
