package mapper

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

const handoffRoot = "/handoff"

// stubHandoffReader answers from memory with the same contract the real reader
// implements: a bounded read that reports the file's real size and whether the
// bytes were truncated at the gate.
type stubHandoffReader struct {
	files map[string][]byte
	err   error
	paths []string
}

func (s *stubHandoffReader) ReadHandoffImage(_ context.Context, path string, maxBytes int64) (HandoffFile, error) {
	s.paths = append(s.paths, path)

	if s.err != nil {
		return HandoffFile{}, s.err
	}

	data, ok := s.files[path]
	if !ok {
		return HandoffFile{}, &HandoffPathError{Verdict: HandoffMissingFile, Message: "handoff file does not exist"}
	}

	size := int64(len(data))
	if maxBytes > 0 && size > maxBytes {
		return HandoffFile{Data: data[:maxBytes+1], Size: size, Truncated: true}, nil
	}

	return HandoffFile{Data: data, Size: size}, nil
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

	embedded, err := mapPromptBlocks(
		[]acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimePNG)},
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
		name string
		meta map[string]any
	}{
		{name: "absent", meta: map[string]any{}},
		{name: "not an object", meta: map[string]any{MetaKeyHandoff: "handoff"}},
		{
			name: "missing field",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion: HandoffVersion,
				handoffFieldDigest:  valid[handoffFieldDigest],
			}},
		},
		{
			name: "unknown field",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
				"extra":               true,
			}},
		},
		{
			name: "missing version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   nil,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "unsupported version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   2,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "float version",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   2.0,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "digest not a string",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    1,
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "digest too short",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    "abc",
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "digest not lowercase hex",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    "A" + hex.EncodeToString(make([]byte, 32))[1:],
				handoffFieldSizeBytes: valid[handoffFieldSizeBytes],
			}},
		},
		{
			name: "sizeBytes not a number",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: "12",
			}},
		},
		{
			name: "sizeBytes negative",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: -1,
			}},
		},
		{
			name: "sizeBytes negative int64",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: int64(-1),
			}},
		},
		{
			name: "sizeBytes fractional",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: 1.5,
			}},
		},
		{
			name: "sizeBytes out of range",
			meta: map[string]any{MetaKeyHandoff: map[string]any{
				handoffFieldVersion:   HandoffVersion,
				handoffFieldDigest:    valid[handoffFieldDigest],
				handoffFieldSizeBytes: math.MaxFloat64,
			}},
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
			requireHandoffError(t, err, errInvalidHandoff)
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
		name string
		uri  *string
	}{
		{name: "absent", uri: nil},
		{name: "blank", uri: &blank},
		{name: "unparseable", uri: &unparseable},
		{name: "not a file uri", uri: ptr("https://example.test/a.png")},
		{name: "foreign host", uri: &remote},
		{name: "relative path", uri: &opaque},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block := handoffImageBlock("a.png", png, mimePNG)
			block.Image.Uri = test.uri

			// Intent comes from the envelope here, so a defective uri is still a
			// handoff verdict rather than missing data.
			_, err := mapHandoffPrompt(t, block, newStubHandoffReader("a.png", png), ImageInputLimits{})
			requireHandoffError(t, err, errInvalidHandoff)
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

	cases := []struct {
		name    string
		verdict string
	}{
		{name: "outside the root", verdict: HandoffPathNotAllowed},
		{name: "vanished inside the root", verdict: HandoffMissingFile},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			reader := &stubHandoffReader{err: &HandoffPathError{Verdict: test.verdict, Message: "refused"}}
			_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), reader, ImageInputLimits{})
			details := requireHandoffError(t, err, test.verdict)
			require.Equal(t, "refused", details[keyMessage])
		})
	}

	// A reader failure that is not a path refusal (a cancelled turn) is
	// surfaced as itself rather than relabelled as a block defect.
	cancelled := errors.New("context canceled")
	reader := &stubHandoffReader{err: cancelled}
	_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), reader, ImageInputLimits{})
	require.ErrorIs(t, err, cancelled)
}

func TestHandoffImageDigestMismatch(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	tampered := make([]byte, len(png))
	copy(tampered, png)
	tampered[len(tampered)-1] ^= 0xFF

	// Bytes that hash to something else are rejected, never forwarded.
	_, err := mapHandoffPrompt(
		t,
		handoffImageBlock("a.png", png, mimePNG),
		newStubHandoffReader("a.png", tampered),
		ImageInputLimits{},
	)
	details := requireHandoffError(t, err, errHandoffDigestMismatch)
	require.Equal(t, "handoff file bytes do not match the declared digest", details[keyMessage])

	// A declared size that disagrees with the file is the same fail-closed
	// verdict, checked before the hash.
	_, err = mapHandoffPrompt(
		t,
		handoffImageBlock("a.png", png, mimePNG),
		newStubHandoffReader("a.png", png[:len(png)-1]),
		ImageInputLimits{},
	)
	details = requireHandoffError(t, err, errHandoffDigestMismatch)
	require.Equal(t, "handoff file size does not match the declared sizeBytes", details[keyMessage])
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

func TestHandoffImageTruncatedFileSkipsDigestAndRejects(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	limit := int64(len(png)) - 10

	// The read stopped one byte past the gate, so the digest cannot be
	// verified. Nothing unverified is forwarded: the byte gate rejects it.
	block := handoffImageBlock("a.png", png, mimePNG)
	block.Image.Meta = map[string]any{MetaKeyHandoff: map[string]any{
		handoffFieldVersion:   HandoffVersion,
		handoffFieldDigest:    hex.EncodeToString(make([]byte, 32)),
		handoffFieldSizeBytes: 0,
	}}

	_, err := mapHandoffPrompt(
		t,
		block,
		newStubHandoffReader("a.png", png),
		ImageInputLimits{MaxBytesPerImage: limit},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, len(png), details[keySizeBytes])
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

// TestHandoffReadIsBoundedWithoutAPolicyGate covers the disabled per-image
// limit: the read is still bounded, and a file past that bound is rejected
// rather than forwarded with an unverifiable digest.
func TestHandoffReadIsBoundedWithoutAPolicyGate(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")

	// With no policy gate the read bound is the decoded-frame clamp.
	require.EqualValues(t, MaxDecodedFrameBytes, handoffReadBound(ImageInputLimits{}))
	require.EqualValues(t, 10, handoffReadBound(ImageInputLimits{MaxBytesPerImage: 10}))

	// A configured gate is never widened by the clamp.
	gates := handoffGates(ImageInputLimits{MaxBytesPerImage: 10}, HandoffFile{Truncated: true})
	require.EqualValues(t, 10, gates.MaxBytesPerImage)

	// The aggregate gate is deliberately left alone.
	gates = handoffGates(ImageInputLimits{MaxBytesPerPrompt: 10}, HandoffFile{Truncated: true})
	require.EqualValues(t, MaxDecodedFrameBytes, gates.MaxBytesPerImage)
	require.EqualValues(t, 10, gates.MaxBytesPerPrompt)

	// A file that fits the clamp is read and verified as usual.
	_, err := mapHandoffPrompt(t, handoffImageBlock("a.png", png, mimePNG), newStubHandoffReader("a.png", png), ImageInputLimits{})
	require.NoError(t, err)

	// A file past the clamp is truncated by the reader, so its digest cannot be
	// verified; the clamp stands in as the byte gate and rejects it.
	_, err = mapHandoffPrompt(
		t,
		handoffImageBlock("a.png", png, mimePNG),
		&clampedHandoffReader{},
		ImageInputLimits{},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, MaxDecodedFrameBytes+1, details[keySizeBytes])
	require.EqualValues(t, MaxDecodedFrameBytes, details[keyMaxBytes])
}

// clampedHandoffReader answers as the real reader does for a file one byte past
// the read bound, without materializing eight megabytes.
type clampedHandoffReader struct{}

func (*clampedHandoffReader) ReadHandoffImage(_ context.Context, _ string, maxBytes int64) (HandoffFile, error) {
	return HandoffFile{
		Data:      append(fixtureHeaderPNG(), make([]byte, 64)...),
		Size:      maxBytes + 1,
		Truncated: true,
	}, nil
}
