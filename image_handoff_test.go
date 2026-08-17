package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

// handoffCauseMessages is the complete set of client-visible handoff messages.
// Every one is a literal in the adapter, so nothing derived from a path, a
// digest, a byte count, or an operating-system error can appear in a verdict.
var handoffCauseMessages = []string{
	"no handoff read root is configured",
	"handoff metadata is missing",
	"handoff metadata must be an object",
	"handoff metadata is missing version, digest, or sizeBytes",
	"handoff metadata carries a field beyond version, digest, and sizeBytes",
	"unsupported handoff metadata version",
	"handoff digest must be 64 lowercase hexadecimal characters",
	"handoff sizeBytes must be a non-negative integer",
	"handoff block carries no uri",
	"handoff uri cannot be parsed",
	"handoff uri scheme must be file",
	"handoff uri host is not local",
	"handoff uri path must be absolute",
	"handoff path is outside the configured handoff root",
	"handoff read root cannot be opened",
	"handoff path is not allowed inside the configured handoff root",
	"handoff file does not exist",
	"handoff file cannot be inspected",
	"handoff path is not a regular file",
	"handoff file cannot be read",
	"handoff file does not match the declared envelope",
}

func requireHandoffRefusal(t *testing.T, err error, verdict string, message string) {
	t.Helper()

	var refused *mapper.HandoffPathError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, verdict, refused.Verdict)
	require.Equal(t, message, refused.Message)
	require.Equal(t, message, refused.Error())
	require.Contains(t, handoffCauseMessages, message)
}

// openHandoffPath drives the real reader and closes whatever it hands back.
func openHandoffPath(t *testing.T, root string, path string) error {
	t.Helper()

	file, err := newHandoffImageReader(root).OpenHandoffImage(context.Background(), path)
	if err == nil {
		require.NoError(t, file.Close())
	}

	return err
}

// readHandoffPath drives the real reader and returns the bytes it admitted.
func readHandoffPath(t *testing.T, root string, path string) ([]byte, error) {
	t.Helper()

	file, err := newHandoffImageReader(root).OpenHandoffImage(context.Background(), path)
	if err != nil {
		return nil, err
	}

	defer func() { require.NoError(t, file.Close()) }()

	return io.ReadAll(file)
}

// handoffBlock builds a conforming handoff-form image block for bytes a host
// says are at path.
func handoffBlock(path string, data []byte) acp.ContentBlock {
	sum := sha256.Sum256(data)
	uri := "file://" + path

	return handoffBlockForURI(uri, data, hex.EncodeToString(sum[:]))
}

func handoffBlockForURI(uri string, data []byte, digest string) acp.ContentBlock {
	return acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     "image",
		MimeType: "image/png",
		Uri:      &uri,
		Meta: map[string]any{mapper.MetaKeyHandoff: map[string]any{
			"version":   1,
			"digest":    digest,
			"sizeBytes": len(data),
		}},
	}}
}

// mapHandoffThroughReader drives the whole read path: the real root-confined
// reader behind the real mapper, which is the only place a filesystem verdict
// and the ACP error envelope it produces are joined.
func mapHandoffThroughReader(
	t *testing.T,
	root string,
	block acp.ContentBlock,
	limits mapper.ImageInputLimits,
) ([]map[string]any, error) {
	t.Helper()

	return mapper.PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{block},
		nil,
		limits,
		newHandoffImageReader(root),
	)
}

func requireHandoffEnvelope(t *testing.T, err error, verdict string) map[string]any {
	t.Helper()

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)

	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, verdict, details["error"])

	// A handoff verdict always names the image block it arrived on, whatever
	// gate produced it.
	require.Equal(t, "prompt.image", details["field"])

	return details
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
	payload := []byte("handoff-image-bytes")

	path := filepath.Join(root, "a.png")
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	// A subpath of any depth is readable.
	nested := filepath.Join(root, "session", "operation")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	nestedPath := filepath.Join(nested, "b.png")
	require.NoError(t, os.WriteFile(nestedPath, payload, 0o600))

	// A symlink with a relative target inside the root resolves like any other
	// name under it.
	link := filepath.Join(root, "link.png")
	require.NoError(t, os.Symlink("a.png", link))

	for _, target := range []string{path, nestedPath, link} {
		data, err := readHandoffPath(t, root, target)
		require.NoError(t, err)
		require.Equal(t, payload, data)
	}
}

// TestHandoffImageReaderRefusesEveryPathThatEscapes covers the containment
// answers the kernel gives through os.Root, including the ones stricter than a
// resolve-then-check pass: an absolute symlink is refused even when its target
// is inside the root.
func TestHandoffImageReaderRefusesEveryPathThatEscapes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()

	inside := filepath.Join(root, "inside.png")
	require.NoError(t, os.WriteFile(inside, []byte("inside bytes"), 0o600))

	outsideFile := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(outsideFile, []byte("out-of-root bytes"), 0o600))

	escaping := filepath.Join(root, "escaping.png")
	require.NoError(t, os.Symlink(outsideFile, escaping))

	absoluteInside := filepath.Join(root, "absolute-inside.png")
	require.NoError(t, os.Symlink(inside, absoluteInside))

	relativeEscape := filepath.Join(root, "relative-escape.png")
	require.NoError(t, os.Symlink(filepath.Join("..", filepath.Base(outside), "secret"), relativeEscape))

	danglingAbsolute := filepath.Join(root, "dangling-absolute.png")
	require.NoError(t, os.Symlink(filepath.Join(outside, "gone"), danglingAbsolute))

	notAllowed := "handoff path is not allowed inside the configured handoff root"

	cases := []struct {
		name    string
		path    string
		verdict string
		message string
	}{
		{name: "outside the root", path: outsideFile, verdict: mapper.HandoffPathNotAllowed, message: notAllowed},
		{
			name:    "traversal out of the root",
			path:    filepath.Join(root, "..", "a.png"),
			verdict: mapper.HandoffPathNotAllowed,
			message: notAllowed,
		},
		{name: "symlink out of the root", path: escaping, verdict: mapper.HandoffPathNotAllowed, message: notAllowed},
		{
			name:    "relative symlink out of the root",
			path:    relativeEscape,
			verdict: mapper.HandoffPathNotAllowed,
			message: notAllowed,
		},
		{
			// os.Root refuses an absolute symlink target whatever it points at,
			// which is stricter than resolving the link and comparing paths.
			name:    "absolute symlink inside the root",
			path:    absoluteInside,
			verdict: mapper.HandoffPathNotAllowed,
			message: notAllowed,
		},
		{
			name:    "dangling absolute symlink",
			path:    danglingAbsolute,
			verdict: mapper.HandoffPathNotAllowed,
			message: notAllowed,
		},
		{
			name:    "absent inside the root",
			path:    filepath.Join(root, "missing.png"),
			verdict: mapper.HandoffMissingFile,
			message: "handoff file does not exist",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			requireHandoffRefusal(t, openHandoffPath(t, root, test.path), test.verdict, test.message)
		})
	}

	// A symlink inside the root whose relative target was cleaned up is the
	// host's own race, not a containment failure.
	dangling := filepath.Join(root, "dangling.png")
	require.NoError(t, os.Symlink("gone.png", dangling))
	requireHandoffRefusal(
		t,
		openHandoffPath(t, root, dangling),
		mapper.HandoffMissingFile,
		"handoff file does not exist",
	)

	// A directory opens but is not something this read can use.
	directory := filepath.Join(root, "dir")
	require.NoError(t, os.Mkdir(directory, 0o700))
	requireHandoffRefusal(
		t,
		openHandoffPath(t, root, directory),
		mapper.HandoffPathNotAllowed,
		"handoff path is not a regular file",
	)

	// A path that is not under the root even lexically cannot be made relative
	// to it.
	requireHandoffRefusal(
		t,
		openHandoffPath(t, root, filepath.Join("relative", "a.png")),
		mapper.HandoffPathNotAllowed,
		"handoff path is outside the configured handoff root",
	)

	// A read root that is not there is a deployment defect, and it fails closed
	// rather than reporting the host's file missing.
	requireHandoffRefusal(
		t,
		openHandoffPath(t, filepath.Join(root, "absent-root"), filepath.Join(root, "absent-root", "a.png")),
		mapper.HandoffPathNotAllowed,
		"handoff read root cannot be opened",
	)

	// A cancelled turn surfaces as itself rather than as a path verdict.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newHandoffImageReader(root).OpenHandoffImage(cancelled, inside)
	require.ErrorIs(t, err, context.Canceled)
}

// TestHandoffHardlinkOutOfTheRootIsReadable pins the measured posture rather
// than an assumed one: os.Root does not bound link counts, so a hardlink under
// the root to a file outside it is readable and the declared digest is what
// makes handed-over bytes trustworthy.
func TestHandoffHardlinkOutOfTheRootIsReadable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()

	payload := []byte("out-of-root bytes reached through a hardlink")
	outsideFile := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(outsideFile, payload, 0o600))

	hardlink := filepath.Join(root, "hard.png")
	require.NoError(t, os.Link(outsideFile, hardlink))

	data, err := readHandoffPath(t, root, hardlink)
	require.NoError(t, err)
	require.Equal(t, payload, data)

	// A host that does not know those bytes cannot use the link: the block it
	// would have to send declares a digest it cannot produce.
	block := handoffBlock(hardlink, []byte("bytes the host claims are there"))
	_, err = mapHandoffThroughReader(t, root, block, mapper.ImageInputLimits{})
	details := requireHandoffEnvelope(t, err, "handoff_digest_mismatch")
	require.Equal(t, "handoff file does not match the declared envelope", details["message"])
}

// TestHandoffUnderDeclaredFileIsRejectedAndForwardsNoBytes drives the whole read
// path against a real file that is far larger than its envelope declares. The
// declaration is inside the byte gate, so the gate cannot be what refuses it;
// only reading to the declaration and finding more can, and the answer names the
// envelope rather than the policy.
func TestHandoffUnderDeclaredFileIsRejectedAndForwardsNoBytes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	gate := int64(4096)

	// Structurally valid PNG bytes, so no sniffing, dimension, or animation
	// gate can claim this block ahead of the byte verdict.
	oversized := make([]byte, gate+1)
	copy(oversized, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	copy(oversized[12:16], "IHDR")
	oversized[19] = 1
	oversized[23] = 1

	path := filepath.Join(root, "oversized.png")
	require.NoError(t, os.WriteFile(path, oversized, 0o600))

	sum := sha256.Sum256(oversized)
	block := handoffBlockForURI("file://"+path, make([]byte, 448), hex.EncodeToString(sum[:]))

	blocks, err := mapHandoffThroughReader(t, root, block, mapper.ImageInputLimits{MaxBytesPerImage: gate})
	require.Nil(t, blocks)

	details := requireHandoffEnvelope(t, err, "handoff_digest_mismatch")
	require.Equal(t, "handoff file does not match the declared envelope", details["message"])

	// No byte count reaches the caller, so neither the file's real size nor the
	// gate it would have been measured against is reported back.
	require.NotContains(t, details, "sizeBytes")
	require.NotContains(t, details, "maxBytes")
}

// TestHandoffURIPathDefectsResolveToLocationVerdicts pins the uri shapes a host
// can spell that are not obviously out of the root: a Windows drive path, and
// traversal hidden behind percent-encoding.
func TestHandoffURIPathDefectsResolveToLocationVerdicts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	png := outputFixtureBytes(t, "valid.png")
	digest := sha256.Sum256(png)
	sum := hex.EncodeToString(digest[:])

	cases := []struct {
		name    string
		uri     string
		verdict string
		message string
	}{
		{
			// A Windows spelling names /C:/... on a POSIX host, which is
			// absolute and therefore a location question, never a pass.
			name:    "windows drive uri",
			uri:     "file:///C:/handoff/a.png",
			verdict: "path_not_allowed",
			message: "handoff path is not allowed inside the configured handoff root",
		},
		{
			name:    "percent-encoded traversal",
			uri:     "file://" + root + "/%2e%2e/%2e%2e/etc/passwd",
			verdict: "path_not_allowed",
			message: "handoff path is not allowed inside the configured handoff root",
		},
		{
			name:    "root itself",
			uri:     "file:///",
			verdict: "path_not_allowed",
			message: "handoff path is not allowed inside the configured handoff root",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffBlockForURI(test.uri, png, sum)
			_, err := mapHandoffThroughReader(t, root, block, mapper.ImageInputLimits{})
			details := requireHandoffEnvelope(t, err, test.verdict)
			require.Equal(t, test.message, details["message"])
		})
	}
}

// TestHandoffMessagesAreConstants drives every handoff verdict against a real
// filesystem and pins the client-visible text: each message is one of the fixed
// strings the adapter declares, and none of them can be read back as a fact
// about the host's filesystem.
func TestHandoffMessagesAreConstants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()

	png := outputFixtureBytes(t, "valid.png")
	digest := sha256.Sum256(png)

	good := filepath.Join(root, "good.png")
	require.NoError(t, os.WriteFile(good, png, 0o600))

	outsideFile := filepath.Join(outside, "outside.png")
	require.NoError(t, os.WriteFile(outsideFile, png, 0o600))

	escape := filepath.Join(root, "escape.png")
	require.NoError(t, os.Symlink(outsideFile, escape))

	directory := filepath.Join(root, "directory.png")
	require.NoError(t, os.Mkdir(directory, 0o700))

	tampered := append([]byte(nil), png...)
	tampered[len(tampered)-1] ^= 0xFF
	tamperedPath := filepath.Join(root, "tampered.png")
	require.NoError(t, os.WriteFile(tamperedPath, tampered, 0o600))

	cases := []struct {
		name    string
		root    string
		block   func() acp.ContentBlock
		verdict string
		message string
	}{
		{
			name:    "unset read root",
			root:    "",
			block:   func() acp.ContentBlock { return handoffBlock(good, png) },
			verdict: "invalid_handoff",
			message: "no handoff read root is configured",
		},
		{
			name: "envelope absent",
			root: root,
			block: func() acp.ContentBlock {
				block := handoffBlock(good, png)
				block.Image.Meta = nil

				return block
			},
			verdict: "invalid_handoff",
			message: "handoff metadata is missing",
		},
		{
			name: "uri is not absolute",
			root: root,
			block: func() acp.ContentBlock {
				return handoffBlockForURI("file:relative.png", png, hex.EncodeToString(digest[:]))
			},
			verdict: "invalid_handoff",
			message: "handoff uri path must be absolute",
		},
		{
			name:    "path outside the root",
			root:    root,
			block:   func() acp.ContentBlock { return handoffBlock(outsideFile, png) },
			verdict: "path_not_allowed",
			message: "handoff path is not allowed inside the configured handoff root",
		},
		{
			name:    "symlink escaping the root",
			root:    root,
			block:   func() acp.ContentBlock { return handoffBlock(escape, png) },
			verdict: "path_not_allowed",
			message: "handoff path is not allowed inside the configured handoff root",
		},
		{
			name:    "not a regular file",
			root:    root,
			block:   func() acp.ContentBlock { return handoffBlock(directory, png) },
			verdict: "path_not_allowed",
			message: "handoff path is not a regular file",
		},
		{
			name:    "path gone from inside the root",
			root:    root,
			block:   func() acp.ContentBlock { return handoffBlock(filepath.Join(root, "vanished.png"), png) },
			verdict: "missing_file",
			message: "handoff file does not exist",
		},
		{
			name:    "bytes tampered with",
			root:    root,
			block:   func() acp.ContentBlock { return handoffBlock(tamperedPath, png) },
			verdict: "handoff_digest_mismatch",
			message: "handoff file does not match the declared envelope",
		},
		{
			name: "declared size disagrees",
			root: root,
			block: func() acp.ContentBlock {
				return handoffBlockForURI("file://"+good, png[:len(png)-1], hex.EncodeToString(digest[:]))
			},
			verdict: "handoff_digest_mismatch",
			message: "handoff file does not match the declared envelope",
		},
	}

	decimalRun := regexp.MustCompile(`[0-9]{3,}`)

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := mapHandoffThroughReader(t, test.root, test.block(), mapper.ImageInputLimits{})
			details := requireHandoffEnvelope(t, err, test.verdict)

			message, ok := details["message"].(string)
			require.True(t, ok)
			require.Equal(t, test.message, message)
			require.Contains(t, handoffCauseMessages, message)

			// Nothing about the host's filesystem survives into the text.
			require.NotContains(t, message, root)
			require.NotContains(t, message, outside)
			require.NotContains(t, message, filepath.Base(good))
			require.NotContains(t, message, hex.EncodeToString(digest[:]))
			require.NotRegexp(t, decimalRun, message)
			require.NotContains(t, message, "no such file or directory")
			require.NotContains(t, message, "escapes from parent")
		})
	}
}

// statFailingFile answers the open seam with a descriptor whose own stat fails,
// which is the only way the mode decision can be denied its input.
type statFailingFile struct {
	io.ReadCloser
}

func (statFailingFile) Stat() (os.FileInfo, error) {
	return nil, errors.New("descriptor cannot be inspected")
}

func TestHandoffImageReaderDescriptorCannotBeInspected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.png")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))

	original := handoffOpenFile

	t.Cleanup(func() { handoffOpenFile = original })

	handoffOpenFile = func(opened *os.Root, name string) (openedHandoffFile, error) {
		file, err := opened.Open(name)
		if err != nil {
			return nil, err
		}

		return statFailingFile{ReadCloser: file}, nil
	}

	requireHandoffRefusal(
		t,
		openHandoffPath(t, root, path),
		mapper.HandoffPathNotAllowed,
		"handoff file cannot be inspected",
	)
}
