package claudeacp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/savid/acp-go-claude/internal/mapper"
)

var (
	handoffEvalSymlinks = filepath.EvalSymlinks
	handoffStat         = os.Stat
	handoffOpen         = os.Open
	handoffReadAll      = io.ReadAll
)

// handoffImageReader reads prompt-image bytes a host handed over as local files
// under its configured root. The root is a read root: this never writes, moves,
// or deletes anything under it, and the bytes it returns are copied into the
// native request rather than referenced, so the host's path never outlives the
// read.
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

// ReadHandoffImage resolves path inside the configured root and reads at most
// maxBytes+1 bytes of it. Containment is checked on the cleaned path, again on
// the symlink-resolved path, and the resolved target must be a regular file.
func (r *handoffImageReader) ReadHandoffImage(
	ctx context.Context,
	path string,
	maxBytes int64,
) (mapper.HandoffFile, error) {
	roots := []string{r.root}
	if !pathWithinAnyRoot(path, roots, false) {
		return mapper.HandoffFile{}, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path is outside the configured handoff root",
		)
	}

	resolved, err := handoffEvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mapper.HandoffFile{}, handoffRefused(mapper.HandoffMissingFile, "handoff file does not exist")
		}

		return mapper.HandoffFile{}, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path cannot be resolved safely",
		)
	}

	if !pathWithinAnyRoot(resolved, roots, true) {
		return mapper.HandoffFile{}, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path escapes the configured handoff root",
		)
	}

	info, err := handoffStat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mapper.HandoffFile{}, handoffRefused(mapper.HandoffMissingFile, "handoff file does not exist")
		}

		return mapper.HandoffFile{}, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff file cannot be inspected",
		)
	}

	if !info.Mode().IsRegular() {
		return mapper.HandoffFile{}, handoffRefused(
			mapper.HandoffPathNotAllowed,
			"handoff path is not a regular file",
		)
	}

	data, err := readHandoffBytes(resolved, maxBytes)
	if err != nil {
		return mapper.HandoffFile{}, err
	}

	if err := ctx.Err(); err != nil {
		return mapper.HandoffFile{}, err
	}

	return mapper.HandoffFile{
		Data:      data,
		Size:      max(info.Size(), int64(len(data))),
		Truncated: maxBytes > 0 && int64(len(data)) > maxBytes,
	}, nil
}

func readHandoffBytes(path string, maxBytes int64) ([]byte, error) {
	file, err := handoffOpen(path)
	if err != nil {
		return nil, handoffRefused(mapper.HandoffMissingFile, "handoff file cannot be opened")
	}
	defer file.Close()

	// One byte past the gate distinguishes a file that fits from one that does
	// not without holding an unbounded read.
	var reader io.Reader = file
	if maxBytes > 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}

	data, err := handoffReadAll(reader)
	if err != nil {
		return nil, handoffRefused(mapper.HandoffMissingFile, "handoff file cannot be read")
	}

	return data, nil
}

func handoffRefused(verdict string, message string) error {
	return &mapper.HandoffPathError{Verdict: verdict, Message: message}
}
