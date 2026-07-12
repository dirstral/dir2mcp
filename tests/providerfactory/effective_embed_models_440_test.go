package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/omniembed"
	"github.com/dirstral/dir2mcp/internal/openai"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
)

// TestEffectiveEmbedModels_EmptyProfileResolvesKindDefault pins issue #440 F2:
// built-in embed profiles (openai/cohere/local/omniembed) ship WITHOUT an
// embed_text_model, so the profile fields are empty. The ingest embed worker and
// the query-side retrieval embedder must both send the SAME concrete model — the
// adapter kind default — not an empty string on one side and a stale
// "mistral-embed" fallback on the other (which would embed the query in a foreign
// vector space from the corpus). EffectiveEmbedModels is the single resolution
// point both sides derive from, so this asserts it returns exactly the model each
// adapter substitutes internally when the profile leaves it blank.
func TestEffectiveEmbedModels_EmptyProfileResolvesKindDefault(t *testing.T) {
	cases := []struct {
		name     string
		kind     provider.Kind
		wantText string
		wantCode string
	}{
		{"openai", provider.KindOpenAI, openai.DefaultEmbedModel, openai.DefaultEmbedModel},
		{"cohere", provider.KindCohere, cohere.DefaultEmbedModel, cohere.DefaultEmbedModel},
		{"omniembed", provider.KindOmniEmbed, omniembed.DefaultModel, omniembed.DefaultModel},
		{"gemini", provider.KindGemini, gemini.DefaultEmbedModel, gemini.DefaultEmbedModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty-model built-in profile: only Kind set (openai/local both
			// use kind:openai — the local profile is exactly this shape).
			p := provider.Profile{Name: tc.name, Kind: tc.kind}
			text, code := providerfactory.EffectiveEmbedModels(p)
			if text != tc.wantText {
				t.Errorf("effective embed text model = %q, want %q (adapter kind default)", text, tc.wantText)
			}
			if code != tc.wantCode {
				t.Errorf("effective embed code model = %q, want %q (adapter kind default)", code, tc.wantCode)
			}
			if text == "" {
				t.Error("effective embed text model is empty; the query side would fall back to the stale mistral-embed default and diverge from ingest (#440 F2)")
			}
			if text == "mistral-embed" {
				t.Errorf("effective embed model resolved to the Mistral default %q for kind %q; that is the asymmetric wrong-provider bug #440 F2 fixes", text, tc.kind)
			}
		})
	}
}

// TestEffectiveEmbedModels_ExplicitProfileWins confirms a profile that DOES carry
// an embed model (e.g. the built-in mistral profile: kind:openai +
// mistral-embed/codestral-embed) is returned verbatim, so unifying on the kind
// default never overrides an operator's explicit model.
func TestEffectiveEmbedModels_ExplicitProfileWins(t *testing.T) {
	p := provider.Profile{
		Name:           "mistral",
		Kind:           provider.KindOpenAI,
		EmbedTextModel: "mistral-embed",
		EmbedCodeModel: "codestral-embed",
	}
	text, code := providerfactory.EffectiveEmbedModels(p)
	if text != "mistral-embed" {
		t.Errorf("effective embed text model = %q, want mistral-embed (explicit profile field)", text)
	}
	if code != "codestral-embed" {
		t.Errorf("effective embed code model = %q, want codestral-embed (explicit profile field)", code)
	}
}
