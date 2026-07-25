package mapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

const handoffRoot = "/handoff"

// stubHandoffReader answers from memory with the same contract the real reader
// implements: it opens by path, refuses what it cannot find, and reports no size
// of its own. Bounding the read is the caller's job, as it is in production.
type stubHandoffReader struct {
	files   map[string][]byte
	err     error
	readErr error
	paths   []string
	// read totals the bytes the caller pulled through every descriptor this
	// reader handed out, which is how a test observes the bound the caller
	// actually applied rather than the one it says it applies.
	read     int
	unclosed int
}

func (s *stubHandoffReader) OpenHandoffImage(_ context.Context, path string) (io.ReadCloser, error) {
	s.paths = append(s.paths, path)

	if s.err != nil {
		return nil, s.err
	}

	data, ok := s.files[path]
	if !ok {
		return nil, &HandoffPathError{Verdict: HandoffMissingFile, Message: "handoff file does not exist"}
	}

	s.unclosed++

	return &stubHandoffFile{reader: bytes.NewReader(data), err: s.readErr, opened: s}, nil
}

type stubHandoffFile struct {
	reader *bytes.Reader
	err    error
	opened *stubHandoffReader
}

func (f *stubHandoffFile) Read(into []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}

	count, err := f.reader.Read(into)
	f.opened.read += count

	return count, err
}

func (f *stubHandoffFile) Close() error {
	f.opened.unclosed--

	return nil
}

func newStubHandoffReader(name string, data []byte) *stubHandoffReader {
	return &stubHandoffReader{files: map[string][]byte{filepath.Join(handoffRoot, name): data}}
}

func handoffURI(name string) *string {
	uri := "file://" + filepath.Join(handoffRoot, name)

	return &uri
}

func handoffEnvelopeMeta(data []byte) map[string]any {
	sum := sha256.Sum256(data)

	return map[string]any{MetaKeyHandoff: map[string]any{
		handoffFieldVersion:   HandoffVersion,
		handoffFieldDigest:    hex.EncodeToString(sum[:]),
		handoffFieldSizeBytes: len(data),
	}}
}

// handoffImageBlock builds a conforming handoff-form image block: empty data, a
// file uri under the root, and a matching envelope.
func handoffImageBlock(name string, data []byte, mimeType string) acp.ContentBlock {
	return acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     typeImage,
		MimeType: mimeType,
		Uri:      handoffURI(name),
		Meta:     handoffEnvelopeMeta(data),
	}}
}

func mapHandoffPrompt(
	t *testing.T,
	block acp.ContentBlock,
	reader HandoffFileReader,
	limits ImageInputLimits,
) ([]map[string]any, error) {
	t.Helper()

	return PromptToClaude(context.Background(), []acp.ContentBlock{block}, nil, limits, reader)
}

func requireHandoffError(t *testing.T, err error, reason string) map[string]any {
	t.Helper()

	details := requireImageInputErrorData(t, err)
	require.Equal(t, reason, details[keyErrorField])
	require.Equal(t, fieldPromptImage, details[keyFieldField])
	require.Equal(t, 0, details[keyIndex])
	require.NotEmpty(t, details[keyMessage])

	return details
}

func TestHandoffImageMatchesEmbeddedForm(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	reader := newStubHandoffReader("a.png", png)

	handoff, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), reader, ImageInputLimits{})
	require.NoError(t, err)

	// The embedded side is offered line-wrapped base64, which is legal on the
	// wire and decodes to the same bytes. Identity therefore has to come from
	// the payload rather than from the two sides being handed one string.
	wrapped := wrapBase64(base64.StdEncoding.EncodeToString(png), 76)
	require.Contains(t, wrapped, "\n")

	embedded, err := mapPromptBlocks(
		[]acp.ContentBlock{acp.ImageBlock(wrapped, mimePNG)},
		nil,
		ImageInputLimits{},
	)
	require.NoError(t, err)

	// A handoff-form turn and an embedded-form turn over the same bytes build
	// byte-identical native content.
	require.Equal(t, embedded, handoff)

	// The host's path never reaches the native request.
	require.NotEmpty(t, reader.paths)
	require.NotContains(t, handoff[0][keySource], keyURI)

	for _, block := range handoff {
		source, ok := block[keySource].(map[string]any)
		require.True(t, ok)
		require.NotContains(t, source[keyData], handoffRoot)
	}
}

func TestHandoffImageUnsetRootIsInvalidHandoff(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), nil, ImageInputLimits{})
	details := requireHandoffError(t, err, errInvalidHandoff)
	require.Equal(t, "no handoff read root is configured", details[keyMessage])
}

func TestHandoffImageEnvelopeDefects(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	digest := handoffEnvelopeMeta(png)[MetaKeyHandoff]

	valid, ok := digest.(map[string]any)
	require.True(t, ok)

	cases := []struct {
		name    string
		meta    map[string]any
		message string
	}{
		{name: "absent", meta: map[string]any{}, message: "handoff metadata is missing"},
		{name: "not an object", meta: map[string]any{MetaKeyHandoff: "handoff"}, message: "handoff metadata must be an object"},
		{
			name: "missing field",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion: HandoffVersion,
				handoffFieldDigest:  valid[handoffFieldDigest],
			}},
			message: "handoff metadata is missing version, digest, or sizeBytes",
		},
		{
			// A field short and a field long is not one defect: the count alone
			// cannot tell them apart, and the answer names which happened.
			name: "unknown field replacing a required one",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion: HandoffVersion,
				handoffFieldDigest:  valid[handoffFieldDigest],
				"extra":             true,
			}},
			message: "handoff metadata is missing version, digest, or sizeBytes",
		},
		{
			name: "unknown field",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
				"extra":               true,
			}},
			message: "handoff metadata carries a field beyond version, digest, and sizeBytes",
		},
		{
			name: "missing version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   nil,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "unsupported handoff metadata version",
		},
		{
			name: "unsupported version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   2,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "unsupported handoff metadata version",
		},
		{
			name: "float version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   2.0,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "unsupported handoff metadata version",
		},
		{
			name: "digest not a string",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    1,
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "handoff digest must be 64 lowercase hexadecimal characters",
		},
		{
			name: "digest too short",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    "abc",
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "handoff digest must be 64 lowercase hexadecimal characters",
		},
		{
			name: "digest not lowercase hex",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    "A" + hex.EncodeToString(make([]byte, 32))[1:],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
			message: "handoff digest must be 64 lowercase hexadecimal characters",
		},
		{
			name: "sizeBytes not a number",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: "12",
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
		{
			name: "sizeBytes negative",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: -1,
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
		{
			name: "sizeBytes negative int64",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: int64(-1),
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
		{
			name: "sizeBytes fractional",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: 1.5,
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
		{
			name: "sizeBytes out of range",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: math.MaxFloat64,
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
		{
			// The int64 boundary itself: math.MaxInt64 is not representable as a
			// float64 and rounds up, so a guard that admits this value converts
			// it to a negative int64.
			name: "sizeBytes at the signed 64-bit boundary",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: math.Pow(2, 63),
			}},
			message: "handoff sizeBytes must be a non-negative integer",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock("a.png", png, mimePNG)
			block.Image.Meta = test.meta

			// Every case still signals handoff intent through the file uri, so
			// none of them may be claimed by missing_data.
			_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
			details := requireHandoffError(t, err, errInvalidHandoff)
			require.Equal(t, test.message, details[keyMessage])
		})
	}
}

func TestHandoffImageURIDefects(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	blank := "   "
	opaque := "file:relative.png"
	remote := "file://example.test/a.png"
	unparseable := "file://%zz"
	localhost := "file://localhost" + filepath.Join(handoffRoot, "a.png")

	cases := []struct {
		name    string
		uri     *string
		message string
	}{
		{name: "absent", uri: nil, message: "handoff block carries no uri"},
		{name: "blank", uri: &blank, message: "handoff block carries no uri"},
		{name: "unparseable", uri: &unparseable, message: "handoff uri cannot be parsed"},
		{name: "not a file uri", uri: ptr("https://example.test/a.png"), message: "handoff uri scheme must be file"},
		{name: "foreign host", uri: &remote, message: "handoff uri host is not local"},
		{name: "relative path", uri: &opaque, message: "handoff uri path must be absolute"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock("a.png", png, mimePNG)
			block.Image.Uri = test.uri

			// Intent comes from the envelope here, so a defective uri is still a
			// handoff verdict rather than missing data.
			_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
			details := requireHandoffError(t, err, errInvalidHandoff)
			require.Equal(t, test.message, details[keyMessage])
		})
	}

	// A localhost authority names the local filesystem and is accepted.
	block := handoffImageBlock("a.png", png, mimePNG)
	block.Image.Uri = &localhost
	_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.NoError(t, err)
}

func TestHandoffImageReadRefusals(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	// A reader's path refusal reaches the client with its verdict and its
	// message intact rather than being relabelled. Which paths produce which
	// verdict is the reader's own behaviour, tested against a real filesystem.
	reader := &stubHandoffReader{err: &HandoffPathError{Verdict: HandoffPathNotAllowed, Message: "refused"}}
	_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), reader, ImageInputLimits{})
	details := requireHandoffError(t, err, HandoffPathNotAllowed)
	require.Equal(t, "refused", details[keyMessage])

	// A reader failure that is not a path refusal (a cancelled turn) is
	// surfaced as itself rather than relabelled as a block defect.
	cancelled := errors.New("context canceled")
	_, err = mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), &stubHandoffReader{err: cancelled}, ImageInputLimits{})
	require.ErrorIs(t, err, cancelled)
}

func TestHandoffImageDigestMismatch(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	tampered := make([]byte, len(png))
	copy(tampered, png)
	tampered[len(tampered)-1] ^= 0xFF

	// Bytes that hash to something else are rejected, never forwarded. Bytes of
	// a different length are rejected by the cheap comparison ahead of the
	// hash. The two take different branches and deliberately give one answer:
	// telling them apart would let a caller sweep sizeBytes and read back the
	// exact length of whatever the block pointed at.
	for _, onDisk := range [][]byte{tampered, png[:len(png)-1]} {
		_, err := mapHandoffPrompt(
			t,
			handoffImageBlock("a.png", png, mimePNG),
			newStubHandoffReader("a.png", onDisk),
			ImageInputLimits{},
		)
		details := requireHandoffError(t, err, errHandoffDigestMismatch)
		require.Equal(t, "handoff file does not match the declared envelope", details[keyMessage])
	}
}

func TestHandoffImageGateChain(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	gif := fixtureBytes(t, "animated.gif")
	mismatch := fixtureBytes(t, "mismatch.png")
	truncated := fixtureBytes(t, "truncated.png")

	cases := []struct {
		name   string
		bytes  []byte
		mime   string
		reason string
	}{
		{name: "media type not allowlisted", bytes: png, mime: "image/bmp", reason: errInvalidMediaType},
		{name: "not a raster", bytes: []byte("not an image at all"), mime: mimePNG, reason: errMediaTypeMismatch},
		{name: "unreadable dimensions", bytes: truncated, mime: mimePNG, reason: errInvalidDimensions},
		{name: "animated", bytes: gif, mime: mimeGIF, reason: errAnimatedUnsupported},
		{name: "declared type disagrees", bytes: mismatch, mime: mimePNG, reason: errMediaTypeMismatch},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock("a.png", test.bytes, test.mime)
			_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", test.bytes), ImageInputLimits{})
			details := requireImageInputErrorData(t, err)
			require.Equal(t, test.reason, details[keyErrorField])
		})
	}
}

func TestHandoffImageByteGates(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	size := int64(len(png))

	// The gate is inclusive: exactly the limit passes.
	_, err := mapHandoffPrompt(
		t,
		handoffImageBlock("a.png", png, mimePNG),
		newStubHandoffReader("a.png", png),
		ImageInputLimits{MaxBytesPerImage: size},
	)
	require.NoError(t, err)

	// One byte under the size rejects, and the rejection reports the file's
	// real size even though the bounded read stopped one byte past the gate.
	_, err = mapHandoffPrompt(
		t,
		handoffImageBlock("a.png", png, mimePNG),
		newStubHandoffReader("a.png", png),
		ImageInputLimits{MaxBytesPerImage: size - 1},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, size, details[keySizeBytes])
	require.EqualValues(t, size-1, details[keyMaxBytes])

	// The aggregate gate charges handoff bytes exactly like embedded bytes.
	jpeg := fixtureBytes(t, "valid.jpg")
	reader := &stubHandoffReader{files: map[string][]byte{
		filepath.Join(handoffRoot, "a.png"): png,
		filepath.Join(handoffRoot, "b.jpg"): jpeg,
	}}
	_, err = PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{
			handoffImageBlock("a.png", png, mimePNG),
			handoffImageBlock("b.jpg", jpeg, mimeJPEG),
		},
		nil,
		ImageInputLimits{MaxBytesPerPrompt: size},
		reader,
	)
	details = requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, 1, details[keyIndex])
	require.EqualValues(t, size+int64(len(jpeg)), details[keySizeBytes])

	// A handoff image and an embedded image share one aggregate budget.
	_, err = PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{
			acp.ImageBlock(base64.StdEncoding.EncodeToString(jpeg), mimeJPEG),
			handoffImageBlock("a.png", png, mimePNG),
		},
		nil,
		ImageInputLimits{MaxBytesPerPrompt: int64(len(jpeg))},
		newStubHandoffReader("a.png", png),
	)
	details = requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, 1, details[keyIndex])
}

func TestHandoffImageIntentSelection(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	encoded := base64.StdEncoding.EncodeToString(png)

	// Embedded data wins over a handoff envelope and a uri, unchanged.
	block := handoffImageBlock("a.png", png, mimePNG)
	block.Image.Data = encoded

	blocks, err := mapHandoffPrompt(t, block, &stubHandoffReader{err: errors.New("must not read")}, ImageInputLimits{})
	require.NoError(t, err)

	source, ok := blocks[0][keySource].(map[string]any)
	require.True(t, ok)
	require.Equal(t, encoded, source[keyData])

	// An envelope alone is handoff intent even with no uri at all.
	envelopeOnly := acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     typeImage,
		MimeType: mimePNG,
		Meta:     handoffEnvelopeMeta(png),
	}}
	_, err = mapHandoffPrompt(t, envelopeOnly, newStubHandoffReader("a.png", png), ImageInputLimits{})
	requireHandoffError(t, err, errInvalidHandoff)

	// A uri that cannot be parsed at all is not handoff intent.
	broken := "://"
	brokenBlock := acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     typeImage,
		MimeType: mimePNG,
		Uri:      &broken,
	}}
	_, err = mapHandoffPrompt(t, brokenBlock, newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.Equal(t, errMissingImageData, requireImageInputErrorData(t, err)[keyErrorField])
}

// fixtureHeaderPNG is a structurally valid PNG header, which is all the gates
// ahead of the byte verdict read.
func fixtureHeaderPNG() []byte {
	header := make([]byte, 33)
	copy(header, magicPNG)
	copy(header[12:16], "IHDR")
	header[19] = 1
	header[23] = 1

	return header
}

func ptr(value string) *string {
	return &value
}

// wrapBase64 breaks an encoded payload into lines the way a host emitting MIME
// base64 would.
func wrapBase64(encoded string, width int) string {
	var wrapped strings.Builder

	for start := 0; start < len(encoded); start += width {
		end := min(start+width, len(encoded))

		wrapped.WriteString(encoded[start:end])
		wrapped.WriteString("\n")
	}

	return wrapped.String()
}

func TestAdvertisedMediaTypesMatchTheGate(t *testing.T) {
	t.Parallel()

	images := PortableImageMIMEs()
	require.Equal(t, []string{mimePNG, mimeJPEG, mimeGIF, mimeWebP}, images)

	for _, mime := range images {
		require.True(t, portableImageMIME(mime))
	}

	require.False(t, portableImageMIME(mimeBMP))

	documents := DocumentMIMEs()
	require.Equal(t, []string{mimePDF}, documents)

	// Both accessors hand out copies, so a caller cannot mutate the gate.
	images[0] = "image/tiff"
	documents[0] = "text/plain"
	require.Equal(t, mimePNG, PortableImageMIMEs()[0])
	require.Equal(t, mimePDF, DocumentMIMEs()[0])
}

func TestHandoffEnvelopeIntegerSizes(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	// A host whose JSON decoder yields int64 rather than float64 is accepted.
	block := handoffImageBlock("a.png", png, mimePNG)
	envelope, ok := block.Image.Meta[MetaKeyHandoff].(map[string]any)
	require.True(t, ok)
	envelope[handoffFieldSizeBytes] = int64(len(png))

	_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.NoError(t, err)

	// So is a float, which is what encoding/json actually produces.
	envelope[handoffFieldSizeBytes] = float64(len(png))
	_, err = mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.NoError(t, err)
}

func TestHandoffPathErrorMessage(t *testing.T) {
	t.Parallel()

	err := &HandoffPathError{Verdict: HandoffMissingFile, Message: "handoff file does not exist"}
	require.EqualError(t, err, "handoff file does not exist")
}

// TestHandoffUnderDeclaredFileEndsTheBlockWithoutReadingIt pins the read to the
// caller's own declaration. The file on disk is one byte past the policy gate
// while the envelope declares 448 bytes and carries the real digest of the whole
// file, so nothing about the block is dishonest except its size: an adapter that
// bounded the read at the gate would read every one of those bytes to say so,
// and would report a size the caller never stood behind.
func TestHandoffUnderDeclaredFileEndsTheBlockWithoutReadingIt(t *testing.T) {
	t.Parallel()

	gate := int64(4096)
	declared := int64(448)
	oversized := append(fixtureHeaderPNG(), make([]byte, gate+1-33)...)

	block := handoffImageBlock("a.png", oversized, mimePNG)
	envelope, ok := block.Image.Meta[MetaKeyHandoff].(map[string]any)
	require.True(t, ok)
	envelope[handoffFieldSizeBytes] = declared

	reader := newStubHandoffReader("a.png", oversized)

	blocks, err := PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{block},
		nil,
		ImageInputLimits{MaxBytesPerImage: gate},
		reader,
	)
	require.Nil(t, blocks)

	// A file bigger than its envelope claims is not the file the digest covers,
	// so the truthful answer is the envelope mismatch and not the byte policy.
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errHandoffDigestMismatch, details[keyErrorField])
	require.NotContains(t, details, keySizeBytes)

	// The block was located and read, but never past one byte more than it
	// declared: the work it committed the adapter to is the work it asked for.
	require.Len(t, reader.paths, 1)
	require.LessOrEqual(t, reader.read, int(declared+1))

	// A second block after the rejection proves the prompt was abandoned rather
	// than continuing with the aggregate charged a stale, smaller number.
	png := fixtureBytes(t, "valid.png")
	pair := &stubHandoffReader{files: map[string][]byte{
		filepath.Join(handoffRoot, "a.png"): oversized,
		filepath.Join(handoffRoot, "b.png"): png,
	}}

	blocks, err = PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{block, handoffImageBlock("b.png", png, mimePNG)},
		nil,
		ImageInputLimits{MaxBytesPerImage: gate, MaxBytesPerPrompt: gate},
		pair,
	)
	require.Nil(t, blocks)
	require.Equal(t, errHandoffDigestMismatch, requireImageInputErrorData(t, err)[keyErrorField])
	require.Equal(t, []string{filepath.Join(handoffRoot, "a.png")}, pair.paths)
}

// TestHandoffDeclaredSizeOverTheGateReadsNothing pins the one byte verdict the
// pre-gate still reaches from the envelope alone. The reader would fail on any
// read and the path is not one it knows, so a too_large answer proves the
// declaration was refused before either could matter.
func TestHandoffDeclaredSizeOverTheGateReadsNothing(t *testing.T) {
	t.Parallel()

	gate := int64(4096)
	oversized := append(fixtureHeaderPNG(), make([]byte, gate+1-33)...)

	reader := &stubHandoffReader{}
	reader.readErr = errors.New("bytes must not be read")

	_, err := PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{handoffImageBlock("absent.png", oversized, mimePNG)},
		nil,
		ImageInputLimits{MaxBytesPerImage: gate},
		reader,
	)

	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, gate+1, details[keySizeBytes])
	require.EqualValues(t, gate, details[keyMaxBytes])
	require.Empty(t, reader.paths)
}

// TestHandoffBlockCountIsCappedWithTheAggregateDisabled pins the count cap as
// the only thing bounding handoff I/O when the per-prompt aggregate is off.
// Every block is a valid PNG far below the per-image gate, so no byte or
// structural gate can produce the rejection.
func TestHandoffBlockCountIsCappedWithTheAggregateDisabled(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	reader := newStubHandoffReader("a.png", png)

	prompt := make([]acp.ContentBlock, 0, maxHandoffBlocksPerPrompt+1)
	for range maxHandoffBlocksPerPrompt + 1 {
		prompt = append(prompt, handoffImageBlock("a.png", png, mimePNG))
	}

	_, err := PromptToClaude(
		context.Background(),
		prompt,
		nil,
		ImageInputLimits{MaxBytesPerPrompt: 0},
		reader,
	)

	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, maxHandoffBlocksPerPrompt, details[keyIndex])
	require.EqualValues(t, maxHandoffBlocksPerPrompt+1, details[keySizeBytes])
	require.EqualValues(t, maxHandoffBlocksPerPrompt, details[keyMaxBytes])

	// The block that crossed the cap cost no read at all.
	require.Len(t, reader.paths, maxHandoffBlocksPerPrompt)

	// Exactly the cap is accepted, so the bound is inclusive and a conforming
	// multi-image turn is unaffected.
	accepted, err := PromptToClaude(
		context.Background(),
		prompt[:maxHandoffBlocksPerPrompt],
		nil,
		ImageInputLimits{MaxBytesPerPrompt: 0},
		newStubHandoffReader("a.png", png),
	)
	require.NoError(t, err)
	require.Len(t, accepted, maxHandoffBlocksPerPrompt)
}

// TestHandoffReadHonoursContextCancellation pins the check between blocks: a
// turn cancelled before validation reaches a block opens nothing at all.
func TestHandoffReadHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	reader := newStubHandoffReader("a.png", png)
	_, err := PromptToClaude(
		cancelled,
		[]acp.ContentBlock{handoffImageBlock("a.png", png, mimePNG)},
		nil,
		ImageInputLimits{},
		reader,
	)
	require.ErrorIs(t, err, context.Canceled)

	// Nothing was opened, so no file was touched and the turn slot is free.
	require.Empty(t, reader.paths)
}

// TestEffectiveInputBoundsAreWhatTheGatesEnforce drives each configured shape
// through the real gate rather than comparing a function to the constant it
// returns.
func TestEffectiveInputBoundsAreWhatTheGatesEnforce(t *testing.T) {
	t.Parallel()

	// A file one byte past the decoded-frame clamp, structurally valid so the
	// byte gate is the only thing that can reject it.
	oversized := append(fixtureHeaderPNG(), make([]byte, MaxDecodedFrameBytes+1-33)...)

	// Neither a disabled per-image limit nor one larger than the frame can
	// widen the bound past the clamp.
	for _, configured := range []int64{0, MaxDecodedFrameBytes + 1, 10_000_000_000} {
		_, err := mapHandoffPrompt(
			t,
			handoffImageBlock("a.png", oversized, mimePNG),
			newStubHandoffReader("a.png", oversized),
			ImageInputLimits{MaxBytesPerImage: configured},
		)
		details := requireImageInputErrorData(t, err)
		require.Equal(t, errImageTooLarge, details[keyErrorField])
		require.EqualValues(t, MaxDecodedFrameBytes, details[keyMaxBytes])
		require.EqualValues(t, EffectiveInputBytesPerImage(configured), details[keyMaxBytes])
	}

	// A zero aggregate enforces no total, so a prompt whose blocks sum past
	// every per-image bound is still accepted.
	png := fixtureBytes(t, "valid.png")
	blocks, err := PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{
			handoffImageBlock("a.png", png, mimePNG),
			handoffImageBlock("a.png", png, mimePNG),
		},
		nil,
		ImageInputLimits{MaxBytesPerImage: int64(len(png)), MaxBytesPerPrompt: 0},
		newStubHandoffReader("a.png", png),
	)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	require.EqualValues(t, 0, EffectiveInputBytesPerPrompt(0))
}

// TestHandoffEnvelopeNumbersAreValidatedAsFloats pins the declared size against
// the float64 boundary rather than against whatever an out-of-range float-to-int
// conversion happens to produce on the host architecture.
func TestHandoffEnvelopeNumbersAreValidatedAsFloats(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	cases := []struct {
		name    string
		size    any
		message string
	}{
		{name: "one past the largest int64", size: math.Pow(2, 63), message: sizeBytesDefect},
		{name: "the largest int64 as a float", size: float64(math.MaxInt64), message: sizeBytesDefect},
		{name: "beyond exact float integers", size: math.Pow(2, 53), message: ""},
		{name: "json number in range", size: json.Number(strconv.Itoa(len(png))), message: "accepted"},
		{name: "json number past the boundary", size: json.Number("9223372036854775808"), message: sizeBytesDefect},
		{name: "json number that is not a number", size: json.Number("not-a-number"), message: sizeBytesDefect},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock("a.png", png, mimePNG)
			envelope, ok := block.Image.Meta[MetaKeyHandoff].(map[string]any)
			require.True(t, ok)
			envelope[handoffFieldSizeBytes] = test.size

			_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})

			switch test.message {
			case "accepted":
				require.NoError(t, err)

				return
			case "":
			default:
				details := requireHandoffError(t, err, errInvalidHandoff)
				require.Equal(t, test.message, details[keyMessage])

				return
			}

			// A size that is a legal integer but larger than the gate is a byte
			// verdict, decided before the file is read at all.
			details := requireImageInputErrorData(t, err)
			require.Equal(t, errImageTooLarge, details[keyErrorField])
			require.EqualValues(t, MaxDecodedFrameBytes, details[keyMaxBytes])
		})
	}
}

const sizeBytesDefect = "handoff sizeBytes must be a non-negative integer"

// TestHandoffEnvelopeSurvivesANumberDecodingDecoder pins the envelope decoders
// against the transport's JSON options: a decoder configured to keep numbers as
// text must not turn a conforming block into a defect.
func TestHandoffEnvelopeSurvivesANumberDecodingDecoder(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	sum := sha256.Sum256(png)

	raw := `{"acp-go.dev/handoff":{"version":1,"digest":"` + hex.EncodeToString(sum[:]) +
		`","sizeBytes":` + strconv.Itoa(len(png)) + `}}`

	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()

	var meta map[string]any
	require.NoError(t, decoder.Decode(&meta))

	envelope, ok := meta[MetaKeyHandoff].(map[string]any)
	require.True(t, ok)
	require.IsType(t, json.Number(""), envelope[handoffFieldVersion])

	block := handoffImageBlock("a.png", png, mimePNG)
	block.Image.Meta = meta

	blocks, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.NoError(t, err)
	require.Len(t, blocks, 1)
}

// TestHandoffVersionZeroIsRejected pins the version gate against the zero value,
// which a host omitting the field would produce after a strict decode.
func TestHandoffVersionZeroIsRejected(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	block := handoffImageBlock("a.png", png, mimePNG)
	envelope, ok := block.Image.Meta[MetaKeyHandoff].(map[string]any)
	require.True(t, ok)
	envelope[handoffFieldVersion] = 0

	_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
	details := requireHandoffError(t, err, errInvalidHandoff)
	require.Equal(t, "unsupported handoff metadata version", details[keyMessage])
}

// TestHandoffMediaTypeIsCheckedBeforeTheFilesystem pins the pre-gate ordering: a
// media type the adapter never accepts costs no filesystem access whatsoever.
// The absent-path case is the decisive one, because the only way to answer it
// with the declared type is to never have looked — an adapter that locates the
// file first reports missing_file and, in doing so, tells the caller its guess
// about the path was wrong.
func TestHandoffMediaTypeIsCheckedBeforeTheFilesystem(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	for _, name := range []string{"a.png", "absent.png"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock(name, png, mimePDF)
			reader := newStubHandoffReader("a.png", png)
			reader.readErr = errors.New("bytes must not be read")

			_, err := mapHandoffPrompt(t, block, reader, ImageInputLimits{})
			details := requireImageInputErrorData(t, err)
			require.Equal(t, errInvalidMediaType, details[keyErrorField])

			// Nothing was opened, so nothing is left open either.
			require.Empty(t, reader.paths)
			require.Zero(t, reader.unclosed)
		})
	}
}

// TestHandoffReadFailureIsAMissingFile pins the verdict for a file that located
// and opened but could not be read through.
func TestHandoffReadFailureIsAMissingFile(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	reader := newStubHandoffReader("a.png", png)
	reader.readErr = errors.New("input/output error")

	_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), reader, ImageInputLimits{})
	details := requireHandoffError(t, err, errMissingFile)
	require.Equal(t, "handoff file cannot be read", details[keyMessage])
	require.Zero(t, reader.unclosed)
}
