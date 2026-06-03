// Package providerfactory constructs concrete model.* adapters from a
// resolved provider.Profile (SPEC 0.7.0 / Design 0001). It is the
// impl-side counterpart to the pure internal/provider resolver: the
// resolver decides *which* profile serves a capability (matrix +
// selection), this package builds the wire client for it.
//
// It is the single place that imports every adapter, keeping
// internal/provider dependency-free.
package providerfactory

import (
	"context"
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/anthropic"
	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/elevenlabs"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/openai"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TTSSynthesizer is the optional text-to-speech surface (matches
// mcp.TTSSynthesizer; declared locally to keep this package free of an
// mcp import). TTS is fail-open per SPEC 8.3.
type TTSSynthesizer interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

func unsupported(p provider.Profile, cap provider.Capability) error {
	return fmt.Errorf("provider kind %q cannot serve %s", p.Kind, cap)
}

// validateEmbedDim enforces SPEC 8.1.6 on the requested output dimension.
// A negative dimension is always CONFIG_INVALID (it would otherwise form a
// distinct embed identity yet behave like "unset" at runtime, where checks
// use > 0). A positive (fixed) dimension is only honored by
// Matryoshka-capable kinds (Gemini today); requesting it on any other kind
// is CONFIG_INVALID rather than silently ignored.
func validateEmbedDim(p provider.Profile) error {
	if p.EmbedTextDim < 0 || p.EmbedCodeDim < 0 {
		return fmt.Errorf("CONFIG_INVALID: embed.text_dim/code_dim must be non-negative (got text=%d code=%d)", p.EmbedTextDim, p.EmbedCodeDim)
	}
	if p.Kind != provider.KindGemini && (p.EmbedTextDim > 0 || p.EmbedCodeDim > 0) {
		return fmt.Errorf("CONFIG_INVALID: embed provider kind %q does not support a fixed output dimension (embed.text_dim/code_dim); remove it or use a Matryoshka-capable provider (gemini)", p.Kind)
	}
	return nil
}

// Embedder builds a model.Embedder for the profile (kinds: openai,
// mistral, cohere, gemini). The per-call model still comes from the
// caller; Default*Model is only a fallback for empty model names.
//
// The embedding output-dimension knob (EmbedTextDim/EmbedCodeDim, SPEC
// 8.1.6) is only honored by Gemini today; requesting it on any other
// kind is rejected as CONFIG_INVALID rather than silently ignored.
func Embedder(p provider.Profile) (model.Embedder, error) {
	// SPEC 8.1.6 dimension validation applies to every kind (negative is
	// always invalid; a fixed positive dim is Gemini-only).
	if err := validateEmbedDim(p); err != nil {
		return nil, err
	}
	switch p.Kind {
	case provider.KindOpenAI:
		c := openai.NewClient(p.BaseURL, p.APIKey)
		if p.EmbedTextModel != "" {
			c.DefaultEmbedModel = p.EmbedTextModel
		}
		return c, nil
	case provider.KindGemini:
		c := gemini.NewClient(p.BaseURL, p.APIKey)
		if p.EmbedTextModel != "" {
			c.DefaultEmbedModel = p.EmbedTextModel
		}
		c.CodeEmbedModel = p.EmbedCodeModel
		c.EmbedTextDim = p.EmbedTextDim
		c.EmbedCodeDim = p.EmbedCodeDim
		return c, nil
	case provider.KindCohere:
		c := cohere.NewClient(p.BaseURL, p.APIKey)
		if p.EmbedTextModel != "" {
			c.DefaultEmbedModel = p.EmbedTextModel
		}
		return c, nil
	case provider.KindMistral:
		// Mistral chat/embed run on the OpenAI-compatible backbone
		// (SPEC 8.1.1 re-expression); the bespoke mistral client keeps
		// only OCR/Voxtral. A `mistral` *profile* therefore uses
		// kind=openai for embed; reaching here means a kind:mistral
		// binding for embed, which the matrix forbids.
		return nil, unsupported(p, provider.CapEmbed)
	default:
		return nil, unsupported(p, provider.CapEmbed)
	}
}

// Generator builds a model.Generator (kinds: openai, gemini, cohere,
// anthropic).
func Generator(p provider.Profile) (model.Generator, error) {
	switch p.Kind {
	case provider.KindOpenAI:
		c := openai.NewClient(p.BaseURL, p.APIKey)
		if p.ChatModel != "" {
			c.DefaultChatModel = p.ChatModel
		}
		return c, nil
	case provider.KindGemini:
		c := gemini.NewClient(p.BaseURL, p.APIKey)
		if p.ChatModel != "" {
			c.DefaultChatModel = p.ChatModel
		}
		return c, nil
	case provider.KindCohere:
		c := cohere.NewClient(p.BaseURL, p.APIKey)
		if p.ChatModel != "" {
			c.DefaultChatModel = p.ChatModel
		}
		return c, nil
	case provider.KindAnthropic:
		c := anthropic.NewClient(p.BaseURL, p.APIKey)
		if p.ChatModel != "" {
			c.DefaultChatModel = p.ChatModel
		}
		return c, nil
	default:
		return nil, unsupported(p, provider.CapChat)
	}
}

// Extractor builds a model.DocumentExtractor for the OCR provider
// (same mistral client as OCR; ingest uses the DocumentExtractor
// interface). Docling is a local extractor, not a provider profile,
// and is selected by ingest independently of this.
func Extractor(p provider.Profile) (model.DocumentExtractor, error) {
	if p.Kind != provider.KindMistral {
		return nil, unsupported(p, provider.CapOCR)
	}
	c := mistral.NewClient(p.BaseURL, p.APIKey)
	if p.OCRModel != "" {
		c.DefaultOCRModel = p.OCRModel
	}
	return c, nil
}

// OCR builds a model.OCR. Only the bespoke `mistral` kind serves OCR
// (SPEC 8.1.2 — there is no OpenAI-compatible OCR endpoint).
func OCR(p provider.Profile) (model.OCR, error) {
	if p.Kind != provider.KindMistral {
		return nil, unsupported(p, provider.CapOCR)
	}
	c := mistral.NewClient(p.BaseURL, p.APIKey)
	if p.OCRModel != "" {
		c.DefaultOCRModel = p.OCRModel
	}
	return c, nil
}

// Transcriber builds a model.Transcriber for STT-capable kinds:
// `mistral` (Voxtral), `elevenlabs` (Scribe), `openai`
// (OpenAI-compatible /v1/audio/transcriptions; endpoint-dependent per
// SPEC 8.1.2 ³ — validated at first use), and `gemini` (native
// generateContent with inline audio, SPEC 8.2).
func Transcriber(p provider.Profile) (model.Transcriber, error) {
	switch p.Kind {
	case provider.KindMistral:
		c := mistral.NewClient(p.BaseURL, p.APIKey)
		if p.STTModel != "" {
			c.DefaultTranscribeModel = p.STTModel
		}
		if lang := strings.TrimSpace(p.STTLanguage); lang != "" {
			c.DefaultTranscribeLanguage = lang
		}
		return c, nil
	case provider.KindElevenLabs:
		return newElevenLabs(p), nil
	case provider.KindOpenAI:
		c := openai.NewClient(p.BaseURL, p.APIKey)
		if p.STTModel != "" {
			c.DefaultSTTModel = p.STTModel
		}
		return c, nil
	case provider.KindGemini:
		c := gemini.NewClient(p.BaseURL, p.APIKey)
		if p.STTModel != "" {
			c.DefaultSTTModel = p.STTModel
		}
		if lang := strings.TrimSpace(p.STTLanguage); lang != "" {
			c.DefaultSTTLanguage = lang
		}
		return c, nil
	default:
		return nil, unsupported(p, provider.CapSTT)
	}
}

// newElevenLabs builds an ElevenLabs client carrying the profile's
// voice / STT model / language / base URL (shared by Transcriber+TTS).
func newElevenLabs(p provider.Profile) *elevenlabs.Client {
	c := elevenlabs.NewClient(p.APIKey, p.TTSVoice)
	if p.BaseURL != "" {
		c.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	}
	if p.STTModel != "" {
		c.TranscribeModel = p.STTModel
	}
	if p.STTLanguage != "" {
		c.TranscribeLanguageCode = p.STTLanguage
	}
	return c
}

// TTS builds a TTSSynthesizer for TTS-capable kinds: `elevenlabs`,
// `openai`, `gemini` (SPEC 8.1.2). openai TTS is endpoint-dependent.
func TTS(p provider.Profile) (TTSSynthesizer, error) {
	switch p.Kind {
	case provider.KindElevenLabs:
		return newElevenLabs(p), nil
	case provider.KindOpenAI:
		c := openai.NewClient(p.BaseURL, p.APIKey)
		if p.TTSModel != "" {
			c.DefaultTTSModel = p.TTSModel
		}
		if p.TTSVoice != "" {
			c.DefaultTTSVoice = p.TTSVoice
		}
		return c, nil
	case provider.KindGemini:
		c := gemini.NewClient(p.BaseURL, p.APIKey)
		if p.TTSModel != "" {
			c.DefaultTTSModel = p.TTSModel
		}
		if p.TTSVoice != "" {
			c.DefaultTTSVoice = p.TTSVoice
		}
		return c, nil
	default:
		return nil, unsupported(p, provider.CapTTS)
	}
}

// Reranker builds a model.Reranker (only `cohere`).
func Reranker(p provider.Profile) (model.Reranker, error) {
	if p.Kind != provider.KindCohere {
		return nil, unsupported(p, provider.CapRerank)
	}
	c := cohere.NewClient(p.BaseURL, p.APIKey)
	if p.RerankModel != "" {
		c.DefaultModel = p.RerankModel
	}
	return c, nil
}
