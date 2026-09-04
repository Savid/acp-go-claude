package claudeacp

import "github.com/savid/acp-go-claude/internal/mapper"

const (
	mediaEnvelopeMetaKey = "acp-go.dev/mediaEnvelope"
	handoffMetaKey       = mapper.MetaKeyHandoff
	handoffVersion       = mapper.HandoffVersion

	metaVersionKey = "version"

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
// maps to a native document block. Both byte bounds come from the functions the
// gates themselves call, never from the configured field, so a bound the
// adapter clamps is advertised as clamped.
func mediaEnvelope(limits ImageLimits) map[string]any {
	return map[string]any{
		envelopeFieldMaxBytes:        mapper.EffectiveInputBytesPerImage(limits.MaxInputBytesPerImage),
		envelopeFieldMaxPromptBytes:  mapper.EffectiveInputBytesPerPrompt(limits.MaxInputBytesPerPrompt),
		envelopeFieldMaxDimension:    mediaEnvelopeMaxDimension,
		envelopeFieldImageFormats:    mapper.PortableImageMIMEs(),
		envelopeFieldDocumentFormats: mapper.DocumentMIMEs(),
	}
}
