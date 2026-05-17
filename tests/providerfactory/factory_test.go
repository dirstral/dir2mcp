package tests

import (
	"errors"
	"testing"

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
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindElevenLabs} {
		if tr, err := providerfactory.Transcriber(prof(k)); err != nil || tr == nil {
			t.Errorf("Transcriber(%s) = %v, %v", k, tr, err)
		}
	}
	// matrix-capable but not yet implemented -> explicit sentinel
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindGemini} {
		_, err := providerfactory.Transcriber(prof(k))
		if !errors.Is(err, providerfactory.ErrCapabilityUnimplemented) {
			t.Errorf("Transcriber(%s) want ErrCapabilityUnimplemented, got %v", k, err)
		}
	}
	if _, err := providerfactory.Transcriber(prof(provider.KindCohere)); err == nil {
		t.Error("Transcriber(cohere) must error (not STT-capable)")
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
