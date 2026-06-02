package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/openai"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
)

func prof(k provider.Kind) provider.Profile {
	return provider.Profile{Name: string(k), Kind: k, APIKey: "key"}
}

func TestEmbedder(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindGemini, provider.KindCohere} {
		if e, err := providerfactory.Embedder(prof(k)); err != nil || e == nil {
			t.Errorf("Embedder(%s) = %v, %v; want non-nil", k, e, err)
		}
	}
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindAnthropic, provider.KindElevenLabs} {
		if _, err := providerfactory.Embedder(prof(k)); err == nil {
			t.Errorf("Embedder(%s) must error (matrix: not capable)", k)
		}
	}
}

func TestGenerator(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindGemini, provider.KindCohere, provider.KindAnthropic} {
		if g, err := providerfactory.Generator(prof(k)); err != nil || g == nil {
			t.Errorf("Generator(%s) = %v, %v", k, g, err)
		}
	}
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindElevenLabs} {
		if _, err := providerfactory.Generator(prof(k)); err == nil {
			t.Errorf("Generator(%s) must error", k)
		}
	}
}

func TestOCR(t *testing.T) {
	if o, err := providerfactory.OCR(prof(provider.KindMistral)); err != nil || o == nil {
		t.Fatalf("OCR(mistral) = %v, %v", o, err)
	}
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindGemini, provider.KindCohere, provider.KindAnthropic, provider.KindElevenLabs} {
		if _, err := providerfactory.OCR(prof(k)); err == nil {
			t.Errorf("OCR(%s) must error (only mistral serves OCR)", k)
		}
	}
}

func TestTranscriber(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindElevenLabs, provider.KindOpenAI, provider.KindGemini} {
		if tr, err := providerfactory.Transcriber(prof(k)); err != nil || tr == nil {
			t.Errorf("Transcriber(%s) = %v, %v", k, tr, err)
		}
	}
	for _, k := range []provider.Kind{provider.KindCohere, provider.KindAnthropic} {
		if _, err := providerfactory.Transcriber(prof(k)); err == nil {
			t.Errorf("Transcriber(%s) must error (not STT-capable)", k)
		}
	}
}

func TestTTS(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindElevenLabs, provider.KindOpenAI, provider.KindGemini} {
		if s, err := providerfactory.TTS(prof(k)); err != nil || s == nil {
			t.Errorf("TTS(%s) = %v, %v", k, s, err)
		}
	}
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindCohere, provider.KindAnthropic} {
		if _, err := providerfactory.TTS(prof(k)); err == nil {
			t.Errorf("TTS(%s) must error (not TTS-capable)", k)
		}
	}
}

func TestReranker(t *testing.T) {
	if r, err := providerfactory.Reranker(prof(provider.KindCohere)); err != nil || r == nil {
		t.Fatalf("Reranker(cohere) = %v, %v", r, err)
	}
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindMistral, provider.KindGemini, provider.KindAnthropic, provider.KindElevenLabs} {
		if _, err := providerfactory.Reranker(prof(k)); err == nil {
			t.Errorf("Reranker(%s) must error", k)
		}
	}
}

func TestModelDefaultsPropagate(t *testing.T) {
	p := provider.Profile{Name: "x", Kind: provider.KindOpenAI, APIKey: "k",
		EmbedTextModel: "my-embed", ChatModel: "my-chat"}
	e, _ := providerfactory.Embedder(p)
	if oc, ok := e.(*openai.Client); !ok || oc.DefaultEmbedModel != "my-embed" {
		t.Fatalf("embed default not propagated: %#v", e)
	}
	g, _ := providerfactory.Generator(p)
	if oc, ok := g.(*openai.Client); !ok || oc.DefaultChatModel != "my-chat" {
		t.Fatalf("chat default not propagated: %#v", g)
	}
}

// TestEmbedder_GeminiDimsAndCodeModel verifies the Matryoshka dimension
// and code-model fields (SPEC 8.1.5/8.1.6) propagate to the gemini client.
func TestEmbedder_GeminiDimsAndCodeModel(t *testing.T) {
	p := provider.Profile{
		Name: "gemini", Kind: provider.KindGemini, APIKey: "k",
		EmbedTextModel: "gemini-embedding-001", EmbedCodeModel: "gemini-embedding-001",
		EmbedTextDim: 1536, EmbedCodeDim: 768,
	}
	e, err := providerfactory.Embedder(p)
	if err != nil {
		t.Fatalf("embedder: %v", err)
	}
	gc, ok := e.(*gemini.Client)
	if !ok {
		t.Fatalf("want *gemini.Client, got %T", e)
	}
	if gc.DefaultEmbedModel != "gemini-embedding-001" || gc.CodeEmbedModel != "gemini-embedding-001" {
		t.Fatalf("models not propagated: %+v", gc)
	}
	if gc.EmbedTextDim != 1536 || gc.EmbedCodeDim != 768 {
		t.Fatalf("dims not propagated: text=%d code=%d", gc.EmbedTextDim, gc.EmbedCodeDim)
	}
}

// TestEmbedder_DimRejectedForNonGemini pins SPEC 8.1.6: a fixed output
// dimension on a provider that can't honor it is CONFIG_INVALID, not
// silently ignored.
func TestEmbedder_DimRejectedForNonGemini(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindCohere} {
		p := provider.Profile{Name: string(k), Kind: k, APIKey: "k", EmbedTextDim: 768}
		if _, err := providerfactory.Embedder(p); err == nil {
			t.Fatalf("kind %q: want CONFIG_INVALID for embed dim, got nil", k)
		}
	}
}
