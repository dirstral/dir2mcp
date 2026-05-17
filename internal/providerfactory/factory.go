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
	"fmt"

	"github.com/dirstral/dir2mcp/internal/anthropic"
	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/elevenlabs"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/openai"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// ErrCapabilityUnimplemented is returned when a profile's kind is marked
// capable by the SPEC 8.1.2 matrix but the adapter does not yet
// implement that capability in this release. Currently this is only
// STT/TTS for kinds `openai` and `gemini` (deferred — see #192 / task
// tracking; the matrix is normative but the wire adapters are pending).
var ErrCapabilityUnimplemented = fmt.Errorf("provider capability not implemented in this release")

func unsupported(p provider.Profile, cap provider.Capability) error {
	return fmt.Errorf("provider kind %q cannot serve %s", p.Kind, cap)
}

// Embedder builds a model.Embedder for the profile (kinds: openai,
// mistral, cohere, gemini). The per-call model still comes from the
// caller; Default*Model is only a fallback for empty model names.
func Embedder(p provider.Profile) (model.Embedder, error) {
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

// Transcriber builds a model.Transcriber. Implemented for `mistral`
// (Voxtral) and `elevenlabs` (Scribe). `openai`/`gemini` are matrix-
// capable but their Transcribe adapters are not yet implemented in this
// release → ErrCapabilityUnimplemented (so callers degrade explicitly
// rather than mis-route STT).
func Transcriber(p provider.Profile) (model.Transcriber, error) {
	switch p.Kind {
	case provider.KindMistral:
		c := mistral.NewClient(p.BaseURL, p.APIKey)
		if p.STTModel != "" {
			c.DefaultTranscribeModel = p.STTModel
		}
		return c, nil
	case provider.KindElevenLabs:
		// elevenlabs.NewClient(apiKey, voiceID); voiceID is TTS-only.
		return elevenlabs.NewClient(p.APIKey, ""), nil
	case provider.KindOpenAI, provider.KindGemini:
		return nil, fmt.Errorf("%w: %s STT", ErrCapabilityUnimplemented, p.Kind)
	default:
		return nil, unsupported(p, provider.CapSTT)
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
