package claudeacp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/savid/acp-go-claude/internal/mapper"
)

// openedHandoffFile is the descriptor the read runs on. Stat comes from the
// descriptor rather than from the path, so the mode decision cannot describe a
// different file than the one that was opened.
type openedHandoffFile interface {
	io.ReadCloser
	Stat() (os.FileInfo, error)
}

var handoffOpenFile = openHandoffFile

// openHandoffFile opens name beneath root. The kernel enforces containment as
// part of this one call, and O_NONBLOCK keeps a FIFO or a device from parking
// the turn's goroutine inside open(2) where no cancellation can reach it.
func openHandoffFile(root *os.Root, name string) (openedHandoffFile, error) {
	return root.OpenFile(name, os.O_RDONLY|handoffOpenFlags, 0)
}

// handoffImageReader opens prompt-image files a host handed over under its
// configured root. The root is a read root: this never writes, moves, or
// deletes anything under it, and the bytes the caller reads are copied into the
// native request rather than referenced, so the host's path never outlives the
// read.
//
// Containment is the kernel's answer, not this code's: every open goes through
// os.Root, which refuses a name that resolves outside the root and refuses an
// absolute symlink outright. It does not bound link counts, so a hardlink under
// the root to a file outside it is readable; the declared digest, not the root,
// is what makes handed-over bytes trustworthy.
type handoffImageReader struct {
	root string
}

var _ mapper.HandoffFileReader = (*handoffImageReader)(nil)

// newHandoffImageReader returns nil when no handoff root is configured, which
// is what makes every handoff-form block fail closed.
func newHandoffImageReader(root string) mapper.HandoffFileReader {
	if root == "" {
		return nil
	}

	return &handoffImageReader{root: root}
}

func validateInputHandoffRoot(root string) error {
	if root == "" || filepath.IsAbs(root) {
		return nil
	}

	return errors.New("InputHandoffRoot must be an absolute path")
}

// OpenHandoffImage opens path inside the configured root and hands the caller a
// regular-file descriptor to read from. The name is made relative lexically and
// every containment question is then answered by the open itself, so there is no
// window between deciding a path is allowed and reading it.
func (r *handoffImageReader) OpenHandoffImage(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name, err := filepath.Rel(r.root, filepath.Clean(path))
	if err != nil {
		return nil, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path is outside the configured handoff root",
		)
	}

	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, handoffRefused(mapper.HandoffPathNotAllowed, "handoff read root cannot be opened")
	}
	defer root.Close()

	file, err := handoffOpenFile(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, handoffRefused(mapper.HandoffMissingFile, "handoff file does not exist")
		}

		return nil, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path is not allowed inside the configured handoff root",
		)
	}

	if err := requireRegularHandoffFile(file); err != nil {
		_ = file.Close()

		return nil, err
	}

	return file, nil
}

// requireRegularHandoffFile refuses everything os.Root admits but this read
// cannot use: a directory, and the FIFOs and device files whose containment
// os.Root deliberately says nothing about.
func requireRegularHandoffFile(file openedHandoffFile) error {
	info, err := file.Stat()
	if err != nil {
		return handoffRefused(mapper.HandoffPathNotAllowed, "handoff file cannot be inspected")
	}

	if !info.Mode().IsRegular() {
		return handoffRefused(mapper.HandoffPathNotAllowed, "handoff path is not a regular file")
	}

	return nil
}

func handoffRefused(verdict string, message string) error {
	return &mapper.HandoffPathError{Verdict: verdict, Message: message}
}
