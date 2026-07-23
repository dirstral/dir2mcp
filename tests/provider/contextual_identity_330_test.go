package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// contextualBaseProfile is the default hosted-Mistral embed profile used across the
// contextual-identity cases: its base_url normalizes to "" and its dims are
// native, so a fresh feature-disabled build produces exactly the 9-field
// identity every legacy recording must migrate to.
func contextualBaseProfile() provider.Profile {
	return provider.Profile{
		Name:           "mistral",
		EmbedTextModel: "mistral-embed",
		EmbedCodeModel: "codestral-embed",
	}
}

// TestEmbedIdentity_ContextualIsTerminalField pins SPEC 8.1.4: the identity
// tuple is provider|base_url|text_model|code_model|text_dim|code_dim|multimodal|
// late_chunking|contextual, with `contextual` TERMINAL and `late_chunking` 8th.
func TestEmbedIdentity_ContextualIsTerminalField(t *testing.T) {
	id := provider.EmbedIdentity(contextualBaseProfile(), false, provider.EmbedContextualOff)
	parts := strings.Split(id, "|")
	if len(parts) != 9 {
		t.Fatalf("identity must have 9 fields, got %d: %q", len(parts), id)
	}
	if parts[7] != "off" {
		t.Errorf("late_chunking must be the 8th field, got %q in %q", parts[7], id)
	}
	if parts[8] != provider.EmbedContextualOff {
		t.Errorf("contextual must be the terminal field, got %q in %q", parts[8], id)
	}
	// The token is opaque: an active generator identity is a single field, so it
	// can never collide with the "|" delimiter.
	active := provider.EmbedIdentity(contextualBaseProfile(), false, provider.ContextualIdentity(provider.ContextualSpec{
		Profile: provider.Profile{Name: "openai"}, Model: "gpt-4o-mini", MaxTokens: 128,
		PromptVersion: "v1", Prompt: "situate|this|chunk",
	}))
	if n := strings.Count(active, "|"); n != 8 {
		t.Fatalf("an active contextual token must stay ONE field (8 pipes), got %d: %q", n, active)
	}
}

// TestEmbedIdentity_MigrationLadder is the load-bearing no-spurious-reindex
// case (SPEC 8.1.4, issue #330): EVERY recorded pre-contextual identity shape
// MUST canonicalize to something that compares EQUAL to what a fresh build with
// contextual retrieval DISABLED computes. If any row of this table regresses,
// every existing corpus in the wild re-embeds on upgrade.
func TestEmbedIdentity_MigrationLadder(t *testing.T) {
	current := provider.EmbedIdentity(contextualBaseProfile(), false, provider.EmbedContextualOff)
	if current != "mistral||mistral-embed|codestral-embed|0|0|off|off|off" {
		t.Fatalf("precondition: fresh feature-disabled identity = %q", current)
	}
	for _, tc := range []struct {
		name     string
		recorded string
	}{
		{"3 fields (pre-8.1.6: no dims/multimodal/late-chunking/contextual)",
			"mistral|mistral-embed|codestral-embed"},
		{"5 fields (pre-8.1.7: dims, no multimodal)",
			"mistral|mistral-embed|codestral-embed|0|0"},
		{"6 fields (pre-late-chunking, #446)",
			"mistral|mistral-embed|codestral-embed|0|0|off"},
		{"7 fields (pre-base_url, #560)",
			"mistral|mistral-embed|codestral-embed|0|0|off|off"},
		{"8 fields (pre-contextual — the shape shipped indexes record today)",
			"mistral||mistral-embed|codestral-embed|0|0|off|off"},
		{"9 fields (already current)",
			"mistral||mistral-embed|codestral-embed|0|0|off|off|off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := provider.VerifyEmbedIdentity(tc.recorded, current); err != nil {
				t.Fatalf("recorded %q must NOT force a reindex against %q: %v", tc.recorded, current, err)
			}
		})
	}
	// A fresh index (no recorded identity) always passes.
	if err := provider.VerifyEmbedIdentity("", current); err != nil {
		t.Errorf("empty recorded identity (fresh index) must pass: %v", err)
	}
}

// TestEmbedIdentity_UnrecognizedFieldCountFailsLoudly pins the other half of the
// SPEC 8.1.4 ladder rule: an unrecognized field count MUST be left unchanged so
// it fails the comparison LOUDLY, rather than being coerced into a false match
// that would silently mix vector spaces.
func TestEmbedIdentity_UnrecognizedFieldCountFailsLoudly(t *testing.T) {
	current := provider.EmbedIdentity(contextualBaseProfile(), false, provider.EmbedContextualOff)
	for _, recorded := range []string{
		"mistral",                                // 1 field
		"mistral|mistral-embed",                  // 2 fields
		"mistral||mistral-embed|codestral-embed", // 4 fields
		"mistral||mistral-embed|codestral-embed|0|0|off|off|off|x", // 10 fields
	} {
		err := provider.VerifyEmbedIdentity(recorded, current)
		if err == nil {
			t.Errorf("unrecognized field count %q must NOT be coerced into a match", recorded)
			continue
		}
		if !strings.Contains(err.Error(), "embed identity changed") {
			t.Errorf("recorded %q: unexpected error %v", recorded, err)
		}
	}
}

// TestEmbedIdentity_ContextualTogglesIdentity pins the correctness mechanism
// (SPEC 8.1.4/8.1.8): turning contextualization on, or changing ANY generator
// input, MUST change the identity so the corpus re-embeds instead of mixing
// contextualized and raw vectors.
func TestEmbedIdentity_ContextualTogglesIdentity(t *testing.T) {
	spec := provider.ContextualSpec{
		Profile:       provider.Profile{Name: "openai"},
		Model:         "gpt-4o-mini",
		MaxTokens:     128,
		PromptVersion: "v1",
		Prompt:        "situate the chunk",
	}
	off := provider.EmbedIdentity(contextualBaseProfile(), false, provider.EmbedContextualOff)
	on := provider.EmbedIdentity(contextualBaseProfile(), false, provider.ContextualIdentity(spec))
	if off == on {
		t.Fatal("enabling contextual retrieval must change the embed identity")
	}
	// An identical spec is stable (no gratuitous re-embed on restart). Compare
	// two INDEPENDENTLY constructed specs rather than calling the function twice
	// on the same value: hashing one value twice would pass even if the token
	// leaked allocation identity or map iteration order into the digest.
	same := provider.ContextualSpec{
		Profile:       provider.Profile{Name: "openai"},
		Model:         "gpt-4o-mini",
		MaxTokens:     128,
		PromptVersion: "v1",
		Prompt:        "situate the chunk",
	}
	if provider.ContextualIdentity(spec) != provider.ContextualIdentity(same) {
		t.Fatal("the contextual token must be deterministic across equal specs")
	}
	base := provider.ContextualIdentity(spec)
	for _, tc := range []struct {
		name  string
		muted provider.ContextualSpec
	}{
		{"provider", func() provider.ContextualSpec {
			s := spec
			s.Profile = provider.Profile{Name: "mistral"}
			return s
		}()},
		{"normalized endpoint", func() provider.ContextualSpec {
			s := spec
			s.Profile = provider.Profile{Name: "openai", Kind: provider.KindOpenAI, BaseURL: "http://gpu-vps:8080/v1"}
			return s
		}()},
		{"model", func() provider.ContextualSpec { s := spec; s.Model = "gpt-4o"; return s }()},
		{"max_tokens", func() provider.ContextualSpec { s := spec; s.MaxTokens = 256; return s }()},
		{"prompt_version", func() provider.ContextualSpec { s := spec; s.PromptVersion = "v2"; return s }()},
		{"effective prompt text", func() provider.ContextualSpec {
			s := spec
			s.Prompt = "situate the chunk (edited override)"
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.ContextualIdentity(tc.muted); got == base {
				t.Fatalf("changing the %s must change the contextual token (%q)", tc.name, got)
			}
		})
	}
}

// TestContextualActive distinguishes the disabled/fail-open token from a real
// generator identity.
func TestContextualActive(t *testing.T) {
	if provider.ContextualActive(provider.EmbedContextualOff) {
		t.Error("the off token must not read as active")
	}
	if provider.ContextualActive("") {
		t.Error("an empty component normalizes to off and must not read as active")
	}
	if !provider.ContextualActive(provider.ContextualIdentity(provider.ContextualSpec{
		Profile: provider.Profile{Name: "openai"}, Model: "gpt-4o-mini",
	})) {
		t.Error("a generator token must read as active")
	}
	if provider.NormalizeEmbedContextual("  ") != provider.EmbedContextualOff {
		t.Error("blank normalizes to off")
	}
}
