package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func initializeMeta(t *testing.T, opts ...Option) map[string]any {
	t.Helper()

	resp, err := NewAgent(opts...).Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)

	return resp.AgentCapabilities.Meta
}

func TestInitializeAdvertisesMediaEnvelope(t *testing.T) {
	t.Parallel()

	meta := initializeMeta(t)

	envelope, ok := meta[mediaEnvelopeMetaKey].(map[string]any)
	require.True(t, ok)

	// The exact advertised shape: no more fields, no fewer.
	require.Equal(t, map[string]any{
		envelopeFieldMaxBytes:        int64(6 * 1024 * 1024),
		envelopeFieldMaxPromptBytes:  int64(6 * 1024 * 1024),
		envelopeFieldMaxDimension:    0,
		envelopeFieldImageFormats:    []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		envelopeFieldDocumentFormats: []string{"application/pdf"},
	}, envelope)

	// The envelope is advertised unconditionally, beside the route literal.
	require.Contains(t, meta, routeMetaKey)

	// Configured limits move the advertised bounds with them.
	configured := initializeMeta(t, WithImageLimits(ImageLimits{
		MaxInputBytesPerImage:  1024,
		MaxInputBytesPerPrompt: 4096,
	}))

	envelope, ok = configured[mediaEnvelopeMetaKey].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 1024, envelope[envelopeFieldMaxBytes])
	require.EqualValues(t, 4096, envelope[envelopeFieldMaxPromptBytes])

	// Nothing in the envelope serializes as null.
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "null")
}

// TestMediaEnvelopeAdvertisesTheBoundTheGateReports drives both advertised byte
// numbers through the real gate. Every comparison is against the number a
// rejection reports, never against the option field the advertisement was built
// from, and the two configured values are distinct so advertising one of them
// twice cannot pass.
func TestMediaEnvelopeAdvertisesTheBoundTheGateReports(t *testing.T) {
	t.Parallel()

	png := outputFixtureBytes(t, "valid.png")
	perImage := int64(len(png))
	perPrompt := 2*perImage - 1

	limits := ImageLimits{MaxInputBytesPerImage: perImage, MaxInputBytesPerPrompt: perPrompt}
	envelope := mediaEnvelope(limits)
	require.NotEqual(t, envelope[envelopeFieldMaxBytes], envelope[envelopeFieldMaxPromptBytes])

	// One image past the per-image bound reports the bound that was advertised.
	details := requireGateRejection(t, limits, oversizedPNG(png, perImage+1))
	require.Equal(t, envelope[envelopeFieldMaxBytes], details[envelopeFieldMaxBytes])

	// Two images that each fit cross the aggregate, and that rejection reports
	// the aggregate that was advertised.
	details = requireGateRejection(t, limits, png, png)
	require.EqualValues(t, 2*perImage, details["sizeBytes"])
	require.Equal(t, envelope[envelopeFieldMaxPromptBytes], details[envelopeFieldMaxBytes])

	// A per-image limit that is disabled or wider than the frame is enforced at
	// the frame clamp, and advertised there too.
	overClamp := oversizedPNG(png, mapper.MaxDecodedFrameBytes+1)

	for _, configured := range []int64{0, mapper.MaxDecodedFrameBytes + 1, 10_000_000_000} {
		clamped := ImageLimits{MaxInputBytesPerImage: configured}
		details = requireGateRejection(t, clamped, overClamp)
		require.Equal(t, mediaEnvelope(clamped)[envelopeFieldMaxBytes], details[envelopeFieldMaxBytes])
		require.EqualValues(t, mapper.MaxDecodedFrameBytes, details[envelopeFieldMaxBytes])
	}

	// A disabled aggregate enforces no total, and says so rather than naming a
	// number a host would have to guess the meaning of.
	require.EqualValues(t, 0, mediaEnvelope(ImageLimits{})[envelopeFieldMaxPromptBytes])
}

// oversizedPNG pads a valid raster to size without disturbing its header, so a
// byte gate is the only thing that can reject the result.
func oversizedPNG(png []byte, size int64) []byte {
	return append(append([]byte(nil), png...), make([]byte, size-int64(len(png)))...)
}

// requireGateRejection maps images through the real prompt gates and returns the
// too_large payload they produced.
func requireGateRejection(t *testing.T, limits ImageLimits, images ...[]byte) map[string]any {
	t.Helper()

	prompt := make([]acp.ContentBlock, 0, len(images))
	for _, image := range images {
		prompt = append(prompt, acp.ImageBlock(base64.StdEncoding.EncodeToString(image), "image/png"))
	}

	_, err := mapper.PromptToClaude(context.Background(), prompt, nil, mapper.ImageInputLimits{
		MaxBytesPerImage:  limits.MaxInputBytesPerImage,
		MaxBytesPerPrompt: limits.MaxInputBytesPerPrompt,
	}, nil)

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)

	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "too_large", details["error"])

	return details
}

// TestAdvertisedMediaTypesAreTheOnesTheGateAccepts pins the advertised format
// list against the allowlist a prompt actually meets.
func TestAdvertisedMediaTypesAreTheOnesTheGateAccepts(t *testing.T) {
	t.Parallel()

	png := outputFixtureBytes(t, "valid.png")
	envelope := mediaEnvelope(ImageLimits{})

	formats, ok := envelope[envelopeFieldImageFormats].([]string)
	require.True(t, ok)

	var requestErr *acp.RequestError

	for _, format := range formats {
		_, formatErr := mapper.PromptToClaude(
			context.Background(),
			[]acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(png), format)},
			nil,
			mapper.ImageInputLimits{},
			nil,
		)
		if formatErr == nil {
			require.Equal(t, "image/png", format)

			continue
		}

		require.ErrorAs(t, formatErr, &requestErr)

		formatDetails, formatOK := requestErr.Data.(map[string]any)
		require.True(t, formatOK)

		// An advertised format is never rejected as unaccepted; PNG bytes
		// declared as another advertised raster fail only on the disagreement.
		require.Equal(t, "media_type_mismatch", formatDetails["error"])
	}

	// A media type the envelope does not advertise is rejected as unaccepted.
	_, err := mapper.PromptToClaude(
		context.Background(),
		[]acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(png), "image/bmp")},
		nil,
		mapper.ImageInputLimits{},
		nil,
	)
	require.ErrorAs(t, err, &requestErr)

	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_media_type", details["error"])
}

func TestInitializeAdvertisesHandoffOnlyWithARoot(t *testing.T) {
	t.Parallel()

	// Absence is the actionable signal that the host's option never arrived.
	require.NotContains(t, initializeMeta(t), handoffMetaKey)

	meta := initializeMeta(t, WithInputHandoffRoot(t.TempDir()))
	require.Equal(t, map[string]any{metaVersionsKey: []int{1}}, meta[handoffMetaKey])

	// The envelope is advertised either way.
	require.Contains(t, meta, mediaEnvelopeMetaKey)
}

func TestInputHandoffRootMustBeAbsolute(t *testing.T) {
	t.Parallel()

	_, err := NewAgent(WithInputHandoffRoot("relative")).Initialize(context.Background(), acp.InitializeRequest{})

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)

	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Contains(t, details[jsonFieldError], "InputHandoffRoot must be an absolute path")
}

// TestPromptHandoffImageNeverForwardsTheHostPath drives a whole turn so the
// assertion covers the native request the harness actually receives.
func TestPromptHandoffImageNeverForwardsTheHostPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	png := outputFixtureBytes(t, "valid.png")
	sum := sha256.Sum256(png)
	path := filepath.Join(root, "session", hex.EncodeToString(sum[:])+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, png, 0o600))

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	session.agent.options.InputHandoffRoot = root
	transport.queryMsgs = []map[string]any{
		{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
	}

	uri := "file://" + path
	handoff := acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     "image",
		MimeType: "image/png",
		Uri:      &uri,
		Meta: map[string]any{mapper.MetaKeyHandoff: map[string]any{
			"version":   1,
			"digest":    hex.EncodeToString(sum[:]),
			"sizeBytes": len(png),
		}},
	}}

	_, err := session.Prompt(context.Background(), PromptRequest(session.id, "test-turn", handoff))
	require.NoError(t, err)

	sent, err := json.Marshal(transport.Sent())
	require.NoError(t, err)

	// The bytes reached the harness, and the host's path did not.
	require.Contains(t, string(sent), base64.StdEncoding.EncodeToString(png))
	require.NotContains(t, string(sent), root)
	require.NotContains(t, string(sent), mapper.MetaKeyHandoff)
}

func TestPromptHandoffImageRejectedWithoutARoot(t *testing.T) {
	t.Parallel()

	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	uri := "file:///srv/handoff/a.png"
	handoff := acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type:     "image",
		MimeType: "image/png",
		Uri:      &uri,
	}}

	_, err := session.Prompt(context.Background(), PromptRequest(session.id, "test-turn", handoff))

	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32602, requestErr.Code)

	details, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_handoff", details["error"])
	require.Equal(t, "prompt.image", details["field"])
}
