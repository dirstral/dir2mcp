package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/colbertrerank"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/omniembed"
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
	for _, k := range []provider.Kind{provider.KindMistral, provider.KindElevenLabs, provider.KindOpenAI, provider.KindGemini, provider.KindWhisper} {
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
	for _, k := range []provider.Kind{provider.KindCohere, provider.KindColBERT} {
		if r, err := providerfactory.Reranker(prof(k)); err != nil || r == nil {
			t.Fatalf("Reranker(%s) = %v, %v", k, r, err)
		}
	}
	for _, k := range []provider.Kind{provider.KindOpenAI, provider.KindMistral, provider.KindGemini, provider.KindAnthropic, provider.KindElevenLabs} {
		if _, err := providerfactory.Reranker(prof(k)); err == nil {
			t.Errorf("Reranker(%s) must error", k)
		}
	}
}

// TestReranker_ColBERTModelPropagates verifies the profile's rerank_model
// overrides the colbert client's default (parity with the cohere path).
func TestReranker_ColBERTModelPropagates(t *testing.T) {
	p := provider.Profile{Name: "colbert", Kind: provider.KindColBERT, BaseURL: "http://localhost:9000", RerankModel: "gte-moderncolbert"}
	r, err := providerfactory.Reranker(p)
	if err != nil || r == nil {
		t.Fatalf("Reranker(colbert) = %v, %v", r, err)
	}
	c, ok := r.(*colbertrerank.Client)
	if !ok {
		t.Fatalf("expected *colbertrerank.Client, got %T", r)
	}
	if c.DefaultModel != "gte-moderncolbert" {
		t.Fatalf("rerank_model not propagated: %q", c.DefaultModel)
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

// TestEmbedder_Multimodal pins SPEC 8.1.7: augment/replace require
// provider=gemini with both embed models = gemini-embedding-2; off and any
// other binding combination behave accordingly.
func TestEmbedder_Multimodal(t *testing.T) {
	good := provider.Profile{
		Name: "gemini", Kind: provider.KindGemini, APIKey: "k",
		EmbedTextModel: "gemini-embedding-2", EmbedCodeModel: "gemini-embedding-2",
	}
	for _, mode := range []string{"augment", "replace"} {
		p := good
		p.EmbedMultimodal = mode
		if _, err := providerfactory.Embedder(p); err != nil {
			t.Errorf("mode %q with gemini-embedding-2 on both axes must be valid: %v", mode, err)
		}
	}
	// off (and empty) is always valid, including on non-gemini providers.
	for _, p := range []provider.Profile{
		{Name: "mistral", Kind: provider.KindOpenAI, APIKey: "k", EmbedMultimodal: "off"},
		{Name: "mistral", Kind: provider.KindOpenAI, APIKey: "k"},
	} {
		if _, err := providerfactory.Embedder(p); err != nil {
			t.Errorf("off/empty multimodal must be valid: %v", err)
		}
	}
	// Invalid combinations are CONFIG_INVALID.
	bad := []provider.Profile{
		{Name: "mistral", Kind: provider.KindOpenAI, APIKey: "k", EmbedTextModel: "mistral-embed", EmbedCodeModel: "mistral-embed", EmbedMultimodal: "augment"},            // wrong kind/model
		{Name: "gemini", Kind: provider.KindGemini, APIKey: "k", EmbedTextModel: "gemini-embedding-2", EmbedCodeModel: "gemini-embedding-001", EmbedMultimodal: "replace"}, // code axis not multimodal
		{Name: "gemini", Kind: provider.KindGemini, APIKey: "k", EmbedTextModel: "gemini-embedding-2", EmbedCodeModel: "gemini-embedding-2", EmbedMultimodal: "bogus"},     // unknown mode
	}
	for i, p := range bad {
		if _, err := providerfactory.Embedder(p); err == nil {
			t.Errorf("bad multimodal config #%d must be CONFIG_INVALID, got nil", i)
		}
	}
}

// TestEmbedder_OmniEmbed pins dir2mcp#334: the self-hosted omniembed kind
// builds a model.Embedder that also satisfies model.MultimodalEmbedder, so
// the embedding worker can embed media chunks off-API (SPEC 8.1.7). It is
// credential-optional (a bare base_url is enough).
func TestEmbedder_OmniEmbed(t *testing.T) {
	p := provider.Profile{
		Name: "omniembed", Kind: provider.KindOmniEmbed,
		BaseURL: "http://gpu-vps:8000", CredentialLess: true,
		EmbedTextModel: "omniembed-v0.1",
	}
	e, err := providerfactory.Embedder(p)
	if err != nil {
		t.Fatalf("Embedder(omniembed) error: %v", err)
	}
	if _, ok := e.(*omniembed.Client); !ok {
		t.Fatalf("Embedder(omniembed) = %T, want *omniembed.Client", e)
	}
	if _, ok := e.(model.MultimodalEmbedder); !ok {
		t.Fatal("omniembed embedder must satisfy model.MultimodalEmbedder (SPEC 8.1.7)")
	}
}

// TestEmbedder_OmniEmbedMultimodal pins dir2mcp#334: augment/replace is
// valid on omniembed when both axes share one (operator-chosen) model, and
// CONFIG_INVALID when the axes diverge — never mixing vector spaces (§8.1.4).
func TestEmbedder_OmniEmbedMultimodal(t *testing.T) {
	base := provider.Profile{
		Name: "omniembed", Kind: provider.KindOmniEmbed,
		BaseURL: "http://gpu-vps:8000", CredentialLess: true,
	}
	for _, mode := range []string{"augment", "replace"} {
		p := base
		p.EmbedTextModel, p.EmbedCodeModel = "omniembed-v0.1", "omniembed-v0.1"
		p.EmbedMultimodal = mode
		if _, err := providerfactory.Embedder(p); err != nil {
			t.Errorf("mode %q with one shared omniembed model must be valid: %v", mode, err)
		}
	}
	// Diverging models on the two axes is CONFIG_INVALID (incomparable spaces).
	bad := base
	bad.EmbedTextModel, bad.EmbedCodeModel = "omniembed-v0.1", "other-model"
	bad.EmbedMultimodal = "augment"
	if _, err := providerfactory.Embedder(bad); err == nil {
		t.Error("omniembed multimodal with mismatched text/code models must be CONFIG_INVALID")
	}
}

// TestEmbedder_NegativeDimRejected pins SPEC 8.1.6: a negative dimension is
// CONFIG_INVALID for every kind, including Gemini (it would otherwise form
// a distinct identity yet behave like "unset" at runtime).
func TestEmbedder_NegativeDimRejected(t *testing.T) {
	for _, k := range []provider.Kind{provider.KindGemini, provider.KindOpenAI} {
		p := provider.Profile{Name: string(k), Kind: k, APIKey: "k", EmbedCodeDim: -1}
		if _, err := providerfactory.Embedder(p); err == nil {
			t.Fatalf("kind %q: want CONFIG_INVALID for negative dim, got nil", k)
		}
	}
}
