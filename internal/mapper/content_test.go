package mapper

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestPromptToClaude(t *testing.T) {
	t.Parallel()

	png := fixtureBase64(t, "valid.png")
	blocks, err := PromptToClaude([]acp.ContentBlock{
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

	blocks, err := PromptToClaude([]acp.ContentBlock{
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

	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.TextBlock("/mcp:server:name\targs"),
		acp.TextBlock("/mcp:server:name"),
	}, []acp.AvailableCommand{{Name: "mcp:server:name"}}, ImageInputLimits{})

	require.NoError(t, err)
	require.Equal(t, "/server:name (MCP)\targs", blocks[0]["text"])
	require.Equal(t, "/server:name (MCP)", blocks[1]["text"])
}

func TestPromptToClaudeLeavesUnadvertisedMCPSlashTextByteIdentical(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
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
	image := acp.ImageBlock(png, "image/png")
	image.Image.Uri = &uriImage
	blocks, err := PromptToClaude([]acp.ContentBlock{
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
			BlobResourceContents: &acp.BlobResourceContents{MimeType: &pdfMime, Blob: "pdf"},
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

	blocks, err := PromptToClaude([]acp.ContentBlock{
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
	_, err := PromptToClaude([]acp.ContentBlock{acp.AudioBlock("abc", "audio/wav")}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	// An empty prompt (zero content blocks) is rejected, not forwarded.
	_, err = PromptToClaude(nil, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	_, err = PromptToClaude([]acp.ContentBlock{{}}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, unsupportedPrompt)

	_, err = PromptToClaude([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "bin"},
		}),
	}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, map[string]any{keyErrorField: errValueUnsupported, keyFieldField: fieldPromptResource})

	_, err = PromptToClaude([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{}),
	}, nil, ImageInputLimits{})
	requireInvalidParams(t, err, map[string]any{keyFieldField: fieldPromptResource, keyErrorField: errMissingResourceData})

	_, err = PromptToClaude(
		[]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image"}}},
		nil,
		ImageInputLimits{},
	)
	requireInvalidParams(t, err, map[string]any{keyFieldField: fieldPromptImage, keyErrorField: errMissingImageData, "index": 0})

	fileURI := "file:///tmp/image.png"
	_, err = PromptToClaude(
		[]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &fileURI}}},
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

			_, err := PromptToClaude([]acp.ContentBlock{acp.ImageBlock(data, test.mimeType)}, nil, test.limits)
			details := requireImageInputErrorData(t, err)
			require.Equal(t, test.reason, details[keyErrorField])
			require.Equal(t, test.index, details["index"])
		})
	}

	imageMime := mimePNG
	_, err := PromptToClaude([]acp.ContentBlock{acp.ResourceBlock(acp.EmbeddedResourceResource{
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

	blocks, err := PromptToClaude(prompt, nil, ImageInputLimits{MaxBytesPerImage: 2000, MaxBytesPerPrompt: 4000})
	require.NoError(t, err)
	require.Len(t, blocks, 6)

	for index, image := range types {
		source, ok := blocks[index+1]["source"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, image.mime, source["media_type"])
	}

	first := fixtureBase64(t, "valid.png")
	second := fixtureBase64(t, "valid.jpg")
	_, err = PromptToClaude(
		[]acp.ContentBlock{acp.ImageBlock(first, mimePNG), acp.ImageBlock(second, mimeJPEG)},
		nil,
		ImageInputLimits{MaxBytesPerImage: 2000, MaxBytesPerPrompt: 2000},
	)
	details := requireImageInputErrorData(t, err)
	require.Equal(t, errImageTooLarge, details[keyErrorField])
	require.Equal(t, 1, details["index"])
	require.EqualValues(t, 2240, details["sizeBytes"])
	require.EqualValues(t, 2000, details["maxBytes"])

	blocks, err = PromptToClaude(
		[]acp.ContentBlock{acp.ImageBlock(first, mimePNG), acp.ImageBlock(second, mimeJPEG)},
		nil,
		ImageInputLimits{},
	)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
}

func fixtureBase64(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "images", name))
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(data)
}
