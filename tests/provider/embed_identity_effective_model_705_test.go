package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// identityField returns the i-th component of p's embed identity.
func identityField(t *testing.T, p provider.Profile, i int) string {
	t.Helper()
	id := provider.EmbedIdentity(p, false, provider.EmbedContextualOff)
	parts := strings.Split(id, "|")
	if i >= len(parts) {
		t.Fatalf("identity %q has no field %d", id, i)
	}
	return parts[i]
}

// TestEmbedIdentity_RecordsEffectiveModel pins issue #705: the identity's model
// components are the models actually sent on the wire, not the raw profile
// fields.
//
// The built-in `openai`, `cohere`, `local` and `omniembed` profiles ship with
// BLANK embed model fields (SPEC 8.1.1) — the adapter substitutes its own
// default. Recording the blank field meant the corpus-lifetime identity named no
// model at all, so the per-axis model fence of 8.1.4 was inert for exactly those
// profiles: change an adapter default (or run a worker built from a binary that
// defaults differently) and the identity still compares equal while the vectors
// come from a different model space.
func TestEmbedIdentity_RecordsEffectiveModel(t *testing.T) {
	cases := []struct {
		name string
		kind provider.Kind
		want string
	}{
		{"openai", provider.KindOpenAI, provider.DefaultOpenAIEmbedModel},
		{"cohere", provider.KindCohere, provider.DefaultCohereEmbedModel},
		{"gemini", provider.KindGemini, provider.DefaultGeminiEmbedModel},
		{"omniembed", provider.KindOmniEmbed, provider.DefaultOmniEmbedModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blank := provider.Profile{Name: tc.name, Kind: tc.kind} // no models set
			for _, field := range []int{2, 3} {                     // text_model, code_model
				if got := identityField(t, blank, field); got != tc.want {
					t.Fatalf("identity field %d = %q, want the effective model %q", field, got, tc.want)
				}
			}
		})
	}
}

// TestEmbedIdentity_ExplicitDefaultIsNotAChange pins the #705/#440-F3 property
// an operator hits first: writing the model that is ALREADY in effect into the
// config must not look like a new vector space. Before the fix the blank profile
// recorded "" and the explicit one recorded "text-embedding-3-small", so simply
// documenting the default demanded a full reindex.
func TestEmbedIdentity_ExplicitDefaultIsNotAChange(t *testing.T) {
	implicit := provider.Profile{Name: "openai", Kind: provider.KindOpenAI}
	explicit := provider.Profile{Name: "openai", Kind: provider.KindOpenAI,
		EmbedTextModel: provider.DefaultOpenAIEmbedModel,
		EmbedCodeModel: provider.DefaultOpenAIEmbedModel}

	idImplicit := provider.EmbedIdentity(implicit, false, provider.EmbedContextualOff)
	idExplicit := provider.EmbedIdentity(explicit, false, provider.EmbedContextualOff)
	if idImplicit != idExplicit {
		t.Fatalf("making the effective default explicit changed the identity:\n implicit %q\n explicit %q",
			idImplicit, idExplicit)
	}
	if err := provider.VerifyEmbedIdentity(idImplicit, idExplicit); err != nil {
		t.Fatalf("no reindex may be demanded for an unchanged effective model: %v", err)
	}
}

// TestEmbedIdentity_DifferentModelsStillDiffer is the other direction: configs
// that genuinely produce different vector spaces must still derive different
// identities. A grace rule that made everything match would be worse than the
// bug it fixes.
func TestEmbedIdentity_DifferentModelsStillDiffer(t *testing.T) {
	base := provider.Profile{Name: "openai", Kind: provider.KindOpenAI,
		EmbedTextModel: "text-embedding-3-small", EmbedCodeModel: "text-embedding-3-small"}

	variants := map[string]provider.Profile{
		"different text model": func() provider.Profile {
			p := base
			p.EmbedTextModel = "text-embedding-3-large"
			return p
		}(),
		"different code model": func() provider.Profile {
			p := base
			p.EmbedCodeModel = "text-embedding-3-large"
			return p
		}(),
		"different dimension": func() provider.Profile {
			p := base
			p.EmbedTextDim = 512
			return p
		}(),
		"different endpoint": func() provider.Profile {
			p := base
			p.BaseURL = "https://proxy.internal/v1"
			return p
		}(),
		"different profile name": func() provider.Profile {
			p := base
			p.Name = "openai-alt"
			return p
		}(),
	}
	idBase := provider.EmbedIdentity(base, false, provider.EmbedContextualOff)
	for name, v := range variants {
		id := provider.EmbedIdentity(v, false, provider.EmbedContextualOff)
		if id == idBase {
			t.Errorf("%s: identities must differ, both are %q", name, id)
		}
		if err := provider.VerifyEmbedIdentity(idBase, id); err == nil {
			t.Errorf("%s: a real vector-space change must be rejected", name)
		}
	}

	// And a same-config pair must NOT differ (no gratuitous churn).
	if provider.EmbedIdentity(base, false, provider.EmbedContextualOff) != idBase {
		t.Fatal("identical profiles must derive identical identities")
	}
}

// TestEmbedIdentity_LegacyBlankModelMigrates is the migration half of #705: a
// corpus whose identity was recorded BEFORE this fix carries "" in the model
// fields, because that is what the built-in profile held. Those vectors were in
// fact produced by the adapter default, so the recording must be read as "the
// adapter default", not as a mismatch — otherwise every openai/cohere/local/
// omniembed corpus would silently re-embed on upgrade.
func TestEmbedIdentity_LegacyBlankModelMigrates(t *testing.T) {
	current := provider.EmbedIdentity(
		provider.Profile{Name: "openai", Kind: provider.KindOpenAI}, false, provider.EmbedContextualOff)

	// Exactly what a pre-#705 build recorded for that profile.
	legacy9 := "openai||||0|0|off|off|off"
	if err := provider.VerifyEmbedIdentity(legacy9, current); err != nil {
		t.Fatalf("a pre-#705 blank-model recording must migrate, not reindex: %v", err)
	}
	if !provider.EmbedIdentityMatches(legacy9, current) {
		t.Fatal("EmbedIdentityMatches must agree with VerifyEmbedIdentity")
	}
	// The same recording through the 8.1.4 field-count ladder (a pre-contextual,
	// 8-field form) must migrate too — both migrations compose.
	if err := provider.VerifyEmbedIdentity("openai||||0|0|off|off", current); err != nil {
		t.Fatalf("ladder + blank-model migration must compose: %v", err)
	}

	// The grace is legacy-only and one-directional. It must not let a blank
	// recording match a DIFFERENT provider/endpoint/dimension…
	for _, bad := range []string{
		"cohere||||0|0|off|off|off",                          // different provider
		"openai|https://proxy.internal/v1|||0|0|off|off|off", // different endpoint
		"openai||||512|0|off|off|off",                        // different dimension
		"openai||||0|0|augment|off|off",                      // different multimodal mode
	} {
		if err := provider.VerifyEmbedIdentity(bad, current); err == nil {
			t.Errorf("recorded %q must not be accepted as compatible with %q", bad, current)
		}
	}

	// …nor may a recorded CONCRETE model match a different concrete model. Only
	// an absent recording is graced, so every identity written from now on
	// enforces the model fence (a changed adapter default or a differently
	// defaulting distributed worker mismatches loudly).
	concrete := "openai||text-embedding-3-large|text-embedding-3-large|0|0|off|off|off"
	if err := provider.VerifyEmbedIdentity(concrete, current); err == nil {
		t.Error("a recorded concrete model must never be graced into matching another model")
	}
	// And the grace is not symmetric: a blank CURRENT identity (which the fix
	// no longer produces for these kinds) must not swallow a concrete recording.
	if provider.EmbedIdentityMatches(concrete, "openai||||0|0|off|off|off") {
		t.Error("the blank-model grace must apply to the RECORDED side only")
	}
}
