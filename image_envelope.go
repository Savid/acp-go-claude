package claudeacp

import "github.com/savid/acp-go-claude/internal/mapper"

const (
	mediaEnvelopeMetaKey = "acp-go.dev/mediaEnvelope"
	handoffMetaKey       = mapper.MetaKeyHandoff
	handoffVersion       = mapper.HandoffVersion

	metaVersionsKey = "versions"

	envelopeFieldMaxBytes        = "maxBytes"
	envelopeFieldMaxPromptBytes  = "maxPromptBytes"
	envelopeFieldMaxDimension    = "maxDimension"
	envelopeFieldImageFormats    = "imageFormats"
	envelopeFieldDocumentFormats = "documentFormats"

	// Claude enforces no native per-dimension pixel bound, so the advertised
	// dimension field is the "no bound" sentinel rather than a number.
	mediaEnvelopeMaxDimension = 0
)

// mediaEnvelope reports the effective inbound media bounds a host can rely on
// before it sends: the per-image and per-prompt decoded byte gates this adapter
// enforces, the media types prompt validation accepts, and the media types it
// maps to a native document block. Every value is read from the same field or
// list the corresponding gate reads.
func mediaEnvelope(limits ImageLimits) map[string]any {
	return map[string]any{
		envelopeFieldMaxBytes:        limits.MaxInputBytesPerImage,
		envelopeFieldMaxPromptBytes:  limits.MaxInputBytesPerPrompt,
		envelopeFieldMaxDimension:    mediaEnvelopeMaxDimension,
		envelopeFieldImageFormats:    mapper.PortableImageMIMEs(),
		envelopeFieldDocumentFormats: mapper.DocumentMIMEs(),
	}
}
