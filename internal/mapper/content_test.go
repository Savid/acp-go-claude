package mapper

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// mapPromptBlocks maps a prompt with no handoff read root configured, which is
// every embedded-form case.
func mapPromptBlocks(
	prompt []acp.ContentBlock,
	advertisedCommands []acp.AvailableCommand,
	limits ImageInputLimits,
) ([]map[string]any, error) {
	return PromptToClaude(context.Background(), prompt, advertisedCommands, limits, nil)
}

func TestPromptToClaude(t *testing.T) {
	t.Parallel()

	png := fixtureBase64(t, "valid.png")
	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		acp.TextBlock("hello"),
		acp.ImageBlock(png, "image/png"),
		acp.ResourceLinkBlock("readme", "file:///tmp/README.md"),
	}, nil, ImageInputLimits{})

	require.NoError(t, err)
	require.Len(t, blocks, 3)
	require.Equal(t, map[string]any{"type": "text", "text": "hello"}, blocks[0])
	require.Equal(t, "image", blocks[1]["type"])
	require.Equal(t, map[string]any{
		"type":       "base64",
		"media_type": "image/png",
		"data":       png,
	}, blocks[1]["source"])
	require.Equal(t, "[@README.md](file:///tmp/README.md)", blocks[2]["text"])
}

func TestPromptToClaudeTextAudienceAnnotations(t *testing.T) {
	t.Parallel()

	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		textBlockWithAudience("model-visible", acp.RoleAssistant),
		textBlockWithAudience("client-only", acp.RoleUser),
		textBlockWithAudience("mixed", acp.RoleUser, acp.RoleAssistant),
		textBlockWithAudience("empty"),
	}, nil, ImageInputLimits{})

	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"type": "text", "text": "model-visible"},
		{"type": "text", "text": "mixed"},
		{"type": "text", "text": "empty"},
	}, blocks)
}

func textBlockWithAudience(text string, audience ...acp.Role) acp.ContentBlock {
	return acp.ContentBlock{Text: &acp.ContentBlockText{
		Type: "text",
		Text: text,
		Annotations: &acp.Annotations{
			Audience: audience,
		},
	}}
}

func TestPromptToClaudeRewritesAdvertisedMCPSlashCommands(t *testing.T) {
	t.Parallel()

	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		acp.TextBlock("/mcp:server:name\targs"),
		acp.TextBlock("/mcp:server:name"),
	}, []acp.AvailableCommand{{Name: "mcp:server:name"}}, ImageInputLimits{})

	require.NoError(t, err)
	require.Equal(t, "/server:name (MCP)\targs", blocks[0]["text"])
	require.Equal(t, "/server:name (MCP)", blocks[1]["text"])
}

func TestPromptToClaudeLeavesUnadvertisedMCPSlashTextByteIdentical(t *testing.T) {
	t.Parallel()

	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		acp.TextBlock("/mcp:server:name args"),
		acp.TextBlock("/mcp:bad\tserver:name"),
		acp.TextBlock("/mcp:server"),
		acp.TextBlock("/mcp::name"),
		acp.TextBlock("/compact"),
		acp.TextBlock("prefix /mcp:server:name"),
	}, []acp.AvailableCommand{
		{Name: "mcp:other:name"},
		{Name: "mcp:server"},
		{Name: "mcp::name"},
		{Name: "mcp:bad server:name"},
	}, ImageInputLimits{})

	require.NoError(t, err)
	require.Equal(t, "/mcp:server:name args", blocks[0]["text"])
	require.Equal(t, "/mcp:bad\tserver:name", blocks[1]["text"])
	require.Equal(t, "/mcp:server", blocks[2]["text"])
	require.Equal(t, "/mcp::name", blocks[3]["text"])
	require.Equal(t, "/compact", blocks[4]["text"])
	require.Equal(t, "prefix /mcp:server:name", blocks[5]["text"])
}

func TestPromptToClaudeEmbeddedResources(t *testing.T) {
	t.Parallel()

	pdfMime := "application/pdf"
	imageMime := "image/jpeg"
	uriImage := "https://example.com/image.png"
	jpeg := fixtureBase64(t, "valid.jpg")
	png := fixtureBase64(t, "valid.png")
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 body"))
	image := acp.ImageBlock(png, "image/png")
	image.Image.Uri = &uriImage
	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Uri: "file:///tmp/a.txt", Text: "body"},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: "inline"},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{MimeType: &imageMime, Blob: jpeg},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{MimeType: &pdfMime, Blob: pdf},
		}),
		image,
	}, nil, ImageInputLimits{})

	require.NoError(t, err)
	require.Len(t, blocks, 6)
	require.Equal(t, "[@a.txt](file:///tmp/a.txt)", blocks[0]["text"])
	require.Equal(t, "image", blocks[1]["type"])
	require.Equal(t, "document", blocks[2]["type"])
	require.Equal(t, "image", blocks[3]["type"])
	require.Equal(t, map[string]any{"type": "base64", "media_type": "image/png", "data": png}, blocks[3]["source"])
	require.Equal(t, "\n<context ref=\"file:///tmp/a.txt\">\nbody\n</context>", blocks[4]["text"])
	require.Equal(t, "\n<context ref=\"\">\ninline\n</context>", blocks[5]["text"])
}

func TestPromptToClaudeResourceLinks(t *testing.T) {
	t.Parallel()

	blocks, err := mapPromptBlocks([]acp.ContentBlock{
		acp.ResourceLinkBlock("ticket", "https://example.com/T-1"),
		acp.ResourceLinkBlock("", "%gh&%ij"),
		acp.ResourceLinkBlock("local", "file://localhost/tmp/a.txt"),
		acp.ResourceLinkBlock("drive", "file:///C:/repo/a.txt"),
		acp.ResourceLinkBlock("remote", "file://example.com/tmp/a.txt"),
		acp.ResourceLinkBlock("zed", "zed://workspace/file.go"),
	}, nil, ImageInputLimits{})

	require.NoError(t, err)
	require.Equal(t, "https://example.com/T-1", blocks[0]["text"])
	require.Equal(t, "%gh&%ij", blocks[1]["text"])
	require.Equal(t, "[@a.txt](file://localhost/tmp/a.txt)", blocks[2]["text"])
	require.Equal(t, "[@a.txt](file:///C:/repo/a.txt)", blocks[3]["text"])
	require.Equal(t, "[@a.txt](file://example.com/tmp/a.txt)", blocks[4]["text"])
	require.Equal(t, "[@file.go](zed://workspace/file.go)", blocks[5]["text"])
	require.Equal(t, "", linkName(""))
	require.Equal(t, "[@file://](file://)", formatURIAsLink("file://"))
}

func TestPromptToClaudeUnsupported(t *testing.T) {
	t.Parallel()

	requireInvalidParams := func(t *testing.T, err error, want map[string]any) {
		t.Helper()

		var reqErr *acp.RequestError
		require.ErrorAs(t, err, &reqErr)
		require.Equal(t, -32602, reqErr.Code)
		require.Equal(t, want, reqErr.Data)
	}

	unsupportedPrompt := map[string]any{keyErrorField: errValueUnsupported, keyFieldField: keyPrompt}

	// Unsupported content fails closed as an invalid parameter (-32602) with
	// the uniform unsupported/field shapes, never a bare internal error.
	_, err := mapPromptBlocks([]acp.ContentBlock{acp.AudioBlock("abc", "audio/wav")}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	// An empty prompt (zero content blocks) is rejected, not forwarded.
	_, err = mapPromptBlocks(nil, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	_, err = mapPromptBlocks([]acp.ContentBlock{{}}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	_, err = mapPromptBlocks([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "bin"},
		}),
	}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, map[string]any{keyErrorField: errValueUnsupported, keyFieldField: fieldPromptResource})

	_, err = mapPromptBlocks([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{}),
	}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, map[string]any{keyFieldField: fieldPromptResource, keyErrorField: errMissingResourceData})

	_, err = mapPromptBlocks(
		[]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image"}}},
		nil,
		ImageInputLimits{},
	)
	requireInvalidParams(t, err, map[string]any{keyFieldField: fieldPromptImage, keyErrorField: errMissingImageData, "index": 0})

	// An empty-data block naming a local file signals handoff intent, so with no
	// handoff read root configured it is rejected as a handoff block rather than
	// as missing data.
	fileURI := "file:///tmp/image.png"
	_, err = mapPromptBlocks(
		[]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &fileURI}}},
		nil,
		ImageInputLimits{},
	)
	requireInvalidParams(t, err, map[string]any{
		keyFieldField: fieldPromptImage,
		keyErrorField: errInvalidHandoff,
		"index":       0,
		keyMessage:    "no handoff read root is configured",
	})

	// A remote uri is not handoff intent, so the block stays missing data.
	remoteURI := "https://example.test/image.png"
	_, err = mapPromptBlocks(
		[]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &remoteURI}}},
		nil,
		ImageInputLimits{},
	)
	requireInvalidParams(t, err, map[string]any{keyFieldField: fieldPromptImage, keyErrorField: errMissingImageData, "index": 0})
}

func TestPromptToClaudeImageValidation(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name     string
		fixture  string
		data     string
		mimeType string
		limits   ImageInputLimits
		reason   string
		index    int
	}

	bmp := make([]byte, 26)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[18:22], 2)
	binary.LittleEndian.PutUint32(bmp[22:26], 3)

	// A recognized PNG whose IHDR carries zero dimensions. The zero-length data
	// keeps width and height unreadable.
	zeroDimPNG := append([]byte(nil), magicPNG...)
	zeroDimPNG = append(zeroDimPNG, 0, 0, 0, 13, 'I', 'H', 'D', 'R')
	zeroDimPNG = append(zeroDimPNG, make([]byte, 13)...)
	zeroDimPNG = append(zeroDimPNG, 0, 0, 0, 0)

	// The same zero-dimension header followed by a chunk claiming a length past
	// the remaining buffer, exercising the chunk-walk bound without a panic.
	oversizedChunkPNG := append([]byte(nil), zeroDimPNG...)
	oversizedChunkPNG = append(oversizedChunkPNG, 0xFF, 0xFF, 0xFF, 0xFF, 't', 'E', 'S', 'T')

	cases := []testCase{
		{name: "invalid base64", data: "%", mimeType: mimePNG, reason: errInvalidBase64},
		{name: "missing MIME", fixture: "valid.png", reason: errInvalidMediaType},
		{name: "noncanonical MIME", fixture: "valid.jpg", mimeType: "image/jpg", reason: errInvalidMediaType},
		{name: "mismatch", fixture: "valid.jpg", mimeType: mimePNG, reason: errMediaTypeMismatch},
		{
			name:     "unrecognized format",
			data:     base64.StdEncoding.EncodeToString([]byte("this is plainly not an image")),
			mimeType: mimePNG,
			reason:   errMediaTypeMismatch,
		},
		{
			name:     "non allowlisted raster",
			data:     base64.StdEncoding.EncodeToString(bmp),
			mimeType: mimePNG,
			reason:   errMediaTypeMismatch,
		},
		{name: "truncated", fixture: "truncated.png", mimeType: mimePNG, reason: errInvalidDimensions},
		{
			name:     "oversized chunk length",
			data:     base64.StdEncoding.EncodeToString(oversizedChunkPNG),
			mimeType: mimePNG,
			reason:   errInvalidDimensions,
		},
		{
			name:     "bad dimensions and mismatched declared type",
			data:     base64.StdEncoding.EncodeToString(zeroDimPNG),
			mimeType: mimeJPEG,
			reason:   errInvalidDimensions,
		},
		{name: "animated GIF", fixture: "animated.gif", mimeType: mimeGIF, reason: errAnimatedUnsupported},
		{name: "animated WebP", fixture: "animated.webp", mimeType: mimeWebP, reason: errAnimatedUnsupported},
		{name: "animated PNG", fixture: "animated-apng.png", mimeType: mimePNG, reason: errAnimatedUnsupported},
		{name: "single frame acTL", fixture: "single-frame-actl.png", mimeType: mimePNG, reason: errAnimatedUnsupported},
		{
			name:     "per image too large",
			fixture:  "valid.png",
			mimeType: mimePNG,
			limits:   ImageInputLimits{MaxBytesPerImage: 447},
			reason:   errImageTooLarge,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			data := test.data
			if test.fixture != "" {
				data = fixtureBase64(t, test.fixture)
			}

			_, err := mapPromptBlocks([]acp.ContentBlock{acp.ImageBlock(data, test.mimeType)}, nil, test.limits)
			details := requireImageInputErrorData(t, err)
			require.Equal(t, test.reason, details[keyErrorField])
			require.Equal(t, test.index, details["index"])
		})
	}

	imageMime := mimePNG
	_, err := mapPromptBlocks([]acp.ContentBlock{acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{
			MimeType: &imageMime,
			Blob:     "%",
		},
	})}, nil, ImageInputLimits{})
	require.Equal(t, errInvalidBase64, requireImageInputErrorData(t, err)[keyErrorField])
}

func requireImageInputErrorData(t *testing.T, err error) map[string]any {
	t.Helper()

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)
	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)

	return details
}

func TestPromptToClaudeImageOrderAndAggregateLimit(t *testing.T) {
	t.Parallel()

	types := []struct {
		file string
		mime string
	}{
		{file: "valid.png", mime: mimePNG},
		{file: "valid.jpg", mime: mimeJPEG},
		{file: "valid.gif", mime: mimeGIF},
		{file: "valid.webp", mime: mimeWebP},
	}
	prompt := make([]acp.ContentBlock, 0, len(types)+2)
	prompt = append(prompt, acp.TextBlock("before"))

	for _, image := range types {
		prompt = append(prompt, acp.ImageBlock(fixtureBase64(t, image.file), image.mime))
	}

	prompt = append(prompt, acp.TextBlock("after"))

	blocks, err := mapPromptBlocks(prompt, nil, ImageInputLimits{MaxBytesPerImage: 2000, MaxBytesPerPrompt: 4000})
	require.NoError(t, err)
	require.Len(t, blocks, 6)

	for index, image := range types {
		source, ok := blocks[index+1]["source"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, image.mime, source["media_type"])
	}

	first := fixtureBase64(t, "valid.png")
	second := fixtureBase64(t, "valid.jpg")
	_, err = mapPromptBlocks(
		[]acp.ContentBlock{acp.ImageBlock(first, mimePNG), acp.ImageBlock(second, mimeJPEG)},
		nil,
		ImageInputLimits{MaxBytesPerImage: 2000, MaxBytesPerPrompt: 2000},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, 1, details["index"])
	require.EqualValues(t, 2240, details["sizeBytes"])
	require.EqualValues(t, 2000, details["maxBytes"])

	blocks, err = mapPromptBlocks(
		[]acp.ContentBlock{acp.ImageBlock(first, mimePNG), acp.ImageBlock(second, mimeJPEG)},
		nil,
		ImageInputLimits{},
	)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
}

func fixtureBase64(t *testing.T, name string) string {
	t.Helper()

	return base64.StdEncoding.EncodeToString(fixtureBytes(t, name))
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "images", name))
	require.NoError(t, err)

	return data
}

func blobResourceBlock(mimeType string, blob string) acp.ContentBlock {
	return acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: &acp.BlobResourceContents{MimeType: &mimeType, Blob: blob},
	})
}

func requireResourceInputErrorData(t *testing.T, err error, index int) map[string]any {
	t.Helper()

	details := requireImageInputErrorData(t, err)
	require.Equal(t, fieldPromptResource, details[keyFieldField])
	require.Equal(t, index, details[keyIndex])

	return details
}

// TestPromptToClaudeBlobResourceIsGated covers the embedded blob channel, which
// forwarded unbounded unvalidated bytes whatever their media type.
func TestPromptToClaudeBlobResourceIsGated(t *testing.T) {
	t.Parallel()

	const perImage = 6_291_456

	limits := ImageInputLimits{MaxBytesPerImage: perImage, MaxBytesPerPrompt: perImage}

	// A document blob larger than the limit the same adapter enforces for an
	// image blob is rejected rather than forwarded.
	oversize := base64.StdEncoding.EncodeToString(make([]byte, 6_295_951))
	_, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(mimePDF, oversize)}, nil, limits)
	details := requireResourceInputErrorData(t, err, 0)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, 6_295_951, details[keySizeBytes])
	require.EqualValues(t, perImage, details[keyMaxBytes])

	// Exactly the limit still passes: the gate is inclusive.
	atLimit := base64.StdEncoding.EncodeToString(make([]byte, perImage))
	blocks, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(mimePDF, atLimit)}, nil, limits)
	require.NoError(t, err)
	require.Equal(t, typeDocument, blocks[0][keyType])

	// Corrupt base64 in a blob is rejected rather than handed to the harness.
	_, err = mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(mimePDF, "%%%%")}, nil, limits)
	require.Equal(t, errInvalidBase64, requireResourceInputErrorData(t, err, 0)[keyErrorField])

	// Document bytes are charged to the per-prompt aggregate alongside images.
	png := fixtureBytes(t, "valid.png")
	document := base64.StdEncoding.EncodeToString(make([]byte, 512))
	aggregate := ImageInputLimits{MaxBytesPerPrompt: int64(len(png)) + 511}
	_, err = mapPromptBlocks([]acp.ContentBlock{
		blobResourceBlock(mimePDF, document),
		acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimePNG),
	}, nil, aggregate)
	details = requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, fieldPromptImage, details[keyFieldField])
	// The document ahead of it consumed media index 0.
	require.Equal(t, 1, details[keyIndex])
	require.EqualValues(t, int64(len(png))+512, details[keySizeBytes])

	// The blob itself reports the running aggregate when it is the block that
	// crosses the budget, at its own media index.
	_, err = mapPromptBlocks([]acp.ContentBlock{
		acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimePNG),
		blobResourceBlock(mimePDF, document),
	}, nil, aggregate)
	details = requireResourceInputErrorData(t, err, 1)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, int64(len(png))+512, details[keySizeBytes])
}

// TestDocumentBlobCarriesTheBytesTheGatesMeasured pins the document payload to
// the bytes validation decoded rather than to the host's spelling of them, the
// same way an image blob is pinned. Forwarding the blob verbatim would hand the
// harness a payload the gates never measured, and would let one document reach it
// under as many spellings as the decoder tolerates.
func TestDocumentBlobCarriesTheBytesTheGatesMeasured(t *testing.T) {
	t.Parallel()

	canonical := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 document bytes"))

	// A host emitting MIME base64 wraps its lines. The decoder accepts that, so
	// only re-encoding makes the payload the harness sees the canonical one.
	for _, spelling := range []string{canonical, wrapBase64(canonical, 16)} {
		blocks, err := mapPromptBlocks(
			[]acp.ContentBlock{blobResourceBlock(mimePDF, spelling)},
			nil,
			ImageInputLimits{},
		)
		require.NoError(t, err)

		source, ok := blocks[0][keySource].(map[string]any)
		require.True(t, ok)
		require.Equal(t, canonical, source[keyData])
	}

	// An encoding whose final character sets bits the decoded length cannot hold
	// is refused rather than quietly reinterpreted, so a payload that means two
	// things to two decoders never becomes one this adapter admitted.
	_, err := mapPromptBlocks(
		[]acp.ContentBlock{blobResourceBlock(mimePDF, "QR==")},
		nil,
		ImageInputLimits{},
	)
	require.Equal(t, errInvalidBase64, requireResourceInputErrorData(t, err, 0)[keyErrorField])
}

// TestPromptToClaudeMediaIndexCountsGatedBlocks pins the index a rejection
// reports as the position among gated media blocks in request order, so a
// document and an image can never both report index 0.
func TestPromptToClaudeMediaIndexCountsGatedBlocks(t *testing.T) {
	t.Parallel()

	png := fixtureBase64(t, "valid.png")
	document := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))

	// Text, resource links, and text resources are not gated media and consume
	// no index; the third gated block is index 2 regardless of them.
	prompt := []acp.ContentBlock{
		acp.TextBlock("before"),
		blobResourceBlock(mimePDF, document),
		acp.ResourceLinkBlock("readme", "file:///tmp/README.md"),
		acp.ImageBlock(png, mimePNG),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: "inline"},
		}),
		acp.ImageBlock("%", mimePNG),
	}

	_, err := mapPromptBlocks(prompt, nil, ImageInputLimits{})
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errInvalidBase64, details[keyErrorField])
	require.Equal(t, 2, details[keyIndex])
}

// TestPromptToClaudeBlobMediaTypeNormalization pins the media-type prefix test
// as case- and parameter-insensitive so a noncanonical raster declaration is
// gated as an image instead of taking an unvalidated channel.
func TestPromptToClaudeBlobMediaTypeNormalization(t *testing.T) {
	t.Parallel()

	png := fixtureBase64(t, "valid.png")

	for _, declared := range []string{"IMAGE/PNG", "Image/Png", " image/png ", "image/png; charset=utf-8", "IMAGE/JPEG"} {
		t.Run(declared, func(t *testing.T) {
			t.Parallel()

			blocks, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(declared, png)}, nil, ImageInputLimits{})
			require.Nil(t, blocks)

			details := requireImageInputErrorData(t, err)
			require.Equal(t, errInvalidMediaType, details[keyErrorField])

			// The reported field names the block the bytes arrived on, not the
			// gate chain the declared media type routed them through.
			require.Equal(t, fieldPromptResource, details[keyFieldField])
		})
	}

	// The exact media type still maps to the native image block.
	blocks, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(mimePNG, png)}, nil, ImageInputLimits{})
	require.NoError(t, err)
	require.Equal(t, typeImage, blocks[0][keyType])

	// A raster defect found inside a resource blob is still a resource
	// rejection, and the same defect on an image block is an image rejection.
	_, err = mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(mimePNG, fixtureBase64(t, "mismatch.png"))}, nil, ImageInputLimits{})
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errMediaTypeMismatch, details[keyErrorField])
	require.Equal(t, fieldPromptResource, details[keyFieldField])

	_, err = mapPromptBlocks([]acp.ContentBlock{acp.ImageBlock(fixtureBase64(t, "mismatch.png"), mimePNG)}, nil, ImageInputLimits{})
	details = requireImageInputErrorData(t, err)
	require.Equal(t, errMediaTypeMismatch, details[keyErrorField])
	require.Equal(t, fieldPromptImage, details[keyFieldField])
}

// TestTextResourceIsChargedToThePromptAggregate pins the text variant against
// the same total as every other inbound payload, so declaring bytes as text
// rather than as a blob buys no extra budget.
func TestTextResourceIsChargedToThePromptAggregate(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	text := strings.Repeat("a", len(png))

	textResource := func(body string) acp.ContentBlock {
		return acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: body},
		})
	}

	// One text resource inside the aggregate is inlined as before.
	blocks, err := mapPromptBlocks(
		[]acp.ContentBlock{textResource(text)},
		nil,
		ImageInputLimits{MaxBytesPerPrompt: int64(len(text))},
	)
	require.NoError(t, err)
	require.Contains(t, blocks[0][keyText], text)

	// One byte more than the aggregate is refused, and the refusal names the
	// block it arrived on.
	_, err = mapPromptBlocks(
		[]acp.ContentBlock{textResource(text)},
		nil,
		ImageInputLimits{MaxBytesPerPrompt: int64(len(text)) - 1},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, fieldPromptResource, details[keyFieldField])
	require.EqualValues(t, len(text), details[keySizeBytes])
	require.EqualValues(t, len(text)-1, details[keyMaxBytes])

	// Text and image bytes share one budget: an image that fits on its own is
	// refused once a text resource ahead of it has been charged.
	_, err = mapPromptBlocks(
		[]acp.ContentBlock{
			textResource(text),
			acp.ImageBlock(base64.StdEncoding.EncodeToString(png), mimePNG),
		},
		nil,
		ImageInputLimits{MaxBytesPerPrompt: int64(len(text) + len(png) - 1)},
	)
	details = requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, fieldPromptImage, details[keyFieldField])
}

// TestDocumentMediaTypeIsNormalized pins the document comparison to the same
// normalization the image routing uses, so a noncanonical spelling reaches the
// document gates instead of the refusal path.
func TestDocumentMediaTypeIsNormalized(t *testing.T) {
	t.Parallel()

	document := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 document bytes"))

	for _, declared := range []string{mimePDF, "APPLICATION/PDF", "application/pdf; charset=binary", " Application/PDF "} {
		blocks, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock(declared, document)}, nil, ImageInputLimits{})
		require.NoError(t, err)
		require.Equal(t, typeDocument, blocks[0][keyType])

		source, ok := blocks[0][keySource].(map[string]any)
		require.True(t, ok)

		// The native block carries the canonical media type whatever the host
		// spelled, so the harness never has to normalize.
		require.Equal(t, mimePDF, source[keyMediaType])
	}

	// A media type that is neither an image nor a document is still refused.
	_, err := mapPromptBlocks([]acp.ContentBlock{blobResourceBlock("application/zip", document)}, nil, ImageInputLimits{})
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errValueUnsupported, details[keyErrorField])
}

// TestEmbeddedImagePastTheFrameClampIsRejectedNotTruncated pins the retention
// bound as a gate: with the policy limit disabled the clamp still decides, and
// an image past it is refused rather than shortened and forwarded.
func TestEmbeddedImagePastTheFrameClampIsRejectedNotTruncated(t *testing.T) {
	t.Parallel()

	oversized := append(fixtureHeaderPNG(), make([]byte, MaxDecodedFrameBytes+1-33)...)
	encoded := base64.StdEncoding.EncodeToString(oversized)

	blocks, err := mapPromptBlocks(
		[]acp.ContentBlock{acp.ImageBlock(encoded, mimePNG)},
		nil,
		ImageInputLimits{},
	)
	require.Nil(t, blocks)

	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.EqualValues(t, len(oversized), details[keySizeBytes])
	require.EqualValues(t, MaxDecodedFrameBytes, details[keyMaxBytes])
}

// TestEmbeddedImageIsForwardedAsCanonicalBase64 pins the payload the harness
// receives to the bytes that passed the gates: two legal host spellings of one
// image build the same native request.
func TestEmbeddedImageIsForwardedAsCanonicalBase64(t *testing.T) {
	t.Parallel()

	png := fixtureBytes(t, "valid.png")
	canonical := base64.StdEncoding.EncodeToString(png)

	spellings := []string{canonical, wrapBase64(canonical, 64), wrapBase64(canonical, 76)}
	require.NotEqual(t, spellings[0], spellings[1])

	for _, spelling := range spellings {
		blocks, err := mapPromptBlocks([]acp.ContentBlock{acp.ImageBlock(spelling, mimePNG)}, nil, ImageInputLimits{})
		require.NoError(t, err)

		source, ok := blocks[0][keySource].(map[string]any)
		require.True(t, ok)
		require.Equal(t, canonical, source[keyData])
	}
}
