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

// TestKindDefaultEmbedModel_MatchesClientDefaults is the drift guard for issue
// #705. The embed identity (SPEC 8.1.4) now records the model that goes on the
// wire, which means the provider package must know each adapter's default. It
// keeps that table as literals so it stays free of any HTTP client dependency —
// this test is what stops the two copies from diverging.
//
// Divergence would be silent and severe: the identity would name a model the
// adapter never sends, which is the very confusion #705 exists to remove.
func TestKindDefaultEmbedModel_MatchesClientDefaults(t *testing.T) {
	cases := []struct {
		kind provider.Kind
		want string
	}{
		{provider.KindOpenAI, openai.DefaultEmbedModel},
		{provider.KindCohere, cohere.DefaultEmbedModel},
		{provider.KindGemini, gemini.DefaultEmbedModel},
		{provider.KindOmniEmbed, omniembed.DefaultModel},
	}
	for _, tc := range cases {
		if got := provider.KindDefaultEmbedModel(tc.kind); got != tc.want {
			t.Errorf("provider.KindDefaultEmbedModel(%q) = %q, but the %s client sends %q",
				tc.kind, got, tc.kind, tc.want)
		}
	}
}

// TestEffectiveEmbedModels_SingleResolutionPath pins that ingest/query (via
// providerfactory) and the embed identity (via provider) resolve the effective
// models through ONE implementation, so they can never disagree about which
// model produced the corpus (#705 acceptance: one shared resolution path).
func TestEffectiveEmbedModels_SingleResolutionPath(t *testing.T) {
	profiles := []provider.Profile{
		{Name: "openai", Kind: provider.KindOpenAI},
		{Name: "cohere", Kind: provider.KindCohere},
		{Name: "gemini", Kind: provider.KindGemini, EmbedTextModel: "gemini-embedding-001"},
		{Name: "omniembed", Kind: provider.KindOmniEmbed, EmbedCodeModel: "custom-code"},
		{Name: "vllm", Kind: provider.KindOpenAI, EmbedTextModel: "bge-m3"},
	}
	for _, p := range profiles {
		factoryText, factoryCode := providerfactory.EffectiveEmbedModels(p)
		providerText, providerCode := provider.EffectiveEmbedModels(p)
		if factoryText != providerText || factoryCode != providerCode {
			t.Fatalf("%s: factory resolved (%q,%q) but the identity resolves (%q,%q)",
				p.Name, factoryText, factoryCode, providerText, providerCode)
		}
		// And the identity records exactly those models, so the recorded
		// identity always names what ingest and retrieval actually send.
		id := provider.EmbedIdentity(p, false, provider.EmbedContextualOff)
		wantPrefix := p.Name + "|" + provider.NormalizeEmbedBaseURL(p) + "|" + providerText + "|" + providerCode + "|"
		if len(id) < len(wantPrefix) || id[:len(wantPrefix)] != wantPrefix {
			t.Fatalf("%s: identity %q does not record the effective models %q/%q",
				p.Name, id, providerText, providerCode)
		}
	}
}
