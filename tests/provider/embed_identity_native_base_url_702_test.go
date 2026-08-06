package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// identityBaseURL extracts the 2nd embed-identity field (the normalized
// base_url) for p.
func identityBaseURL(t *testing.T, p provider.Profile) string {
	t.Helper()
	id := provider.EmbedIdentity(p, false, provider.EmbedContextualOff)
	parts := strings.Split(id, "|")
	if len(parts) < 2 {
		t.Fatalf("identity has no base_url field: %q", id)
	}
	return parts[1]
}

// TestEmbedIdentity_NativeCustomBaseURL pins issue #702 / dirstral-spec#72: a
// custom base_url on a NATIVE-surface profile (gemini, cohere) participates in
// the corpus-lifetime embed identity.
//
// SPEC 8.1.4 rule 1 used to declare base_url "not meaningful" for these kinds,
// but both adapters honor a configured base_url and send the embedding request
// there. Two profiles identical except for endpoint A vs endpoint B therefore
// produced BYTE-IDENTICAL identities, so repointing a corpus at a different
// proxy/gateway passed the identity fence and mixed two vector spaces in one
// index — silently, with no error and quietly degraded results.
func TestEmbedIdentity_NativeCustomBaseURL(t *testing.T) {
	for _, kind := range []provider.Kind{provider.KindGemini, provider.KindCohere} {
		t.Run(string(kind), func(t *testing.T) {
			mk := func(base string) provider.Profile {
				return provider.Profile{Name: string(kind), Kind: kind, BaseURL: base,
					EmbedTextModel: "m", EmbedCodeModel: "m"}
			}
			a := mk("https://proxy-a.internal/v1")
			b := mk("https://proxy-b.internal/v1")

			if got := identityBaseURL(t, a); got == "" {
				t.Fatalf("a custom %s endpoint must appear in the identity, got \"\"", kind)
			}
			idA := provider.EmbedIdentity(a, false, provider.EmbedContextualOff)
			idB := provider.EmbedIdentity(b, false, provider.EmbedContextualOff)
			if idA == idB {
				t.Fatalf("two %s profiles at different endpoints must NOT share an identity: %q", kind, idA)
			}
			// A corpus built on endpoint A must refuse to serve on endpoint B.
			if err := provider.VerifyEmbedIdentity(idA, idB); err == nil {
				t.Fatalf("%s: switching custom endpoints must be rejected, not silently accepted", kind)
			}
			// …and must still match itself (no reindex churn on a stable config).
			if err := provider.VerifyEmbedIdentity(idA, idA); err != nil {
				t.Fatalf("%s: the same custom endpoint must pass: %v", kind, err)
			}
			// Only the endpoint differs, so a same-endpoint pair is equal.
			if provider.EmbedIdentity(mk("https://proxy-a.internal/v1"), false, provider.EmbedContextualOff) != idA {
				t.Fatalf("%s: identical profiles must derive identical identities", kind)
			}
		})
	}
}

// TestEmbedIdentity_NativeHostedDefaultStaysEmpty is the migration half of
// #702: hosted-default gemini/cohere corpora — the overwhelming majority, since
// the built-in profiles ship NO base_url — must keep normalizing to "" so they
// do not spuriously reindex. Only a custom native endpoint sees the one-time
// mismatch, which is the bounded, correct safety action (those are exactly the
// corpora that could hold cross-endpoint vectors).
//
// It doubles as a DRIFT GUARD: the hosted endpoints are compared against what
// the real clients use when no base_url is configured, so changing a client
// default without updating the identity table fails here.
func TestEmbedIdentity_NativeHostedDefaultStaysEmpty(t *testing.T) {
	geminiCompatBase := gemini.NewClient("", "k").BaseURL
	geminiNativeBase := strings.TrimSuffix(geminiCompatBase, "/openai")
	cohereBase := cohere.NewClient("", "k").BaseURL

	cases := []struct {
		kind provider.Kind
		base string
	}{
		{provider.KindGemini, ""}, // the built-in profile ships no base_url
		{provider.KindGemini, geminiCompatBase},
		{provider.KindGemini, geminiNativeBase},
		{provider.KindGemini, geminiNativeBase + "/"}, // rule 3 canonicalization
		{provider.KindCohere, ""},
		{provider.KindCohere, cohereBase},
		{provider.KindCohere, strings.ToUpper("HTTPS://") + strings.TrimPrefix(cohereBase, "https://")},
	}
	for _, tc := range cases {
		p := provider.Profile{Name: string(tc.kind), Kind: tc.kind, BaseURL: tc.base, EmbedTextModel: "m"}
		if got := identityBaseURL(t, p); got != "" {
			t.Errorf("%s @ %q: the hosted default must normalize to \"\" (no spurious reindex), got %q",
				tc.kind, tc.base, got)
		}
	}

	// A pre-#702 corpus on the hosted default recorded "" for base_url, so its
	// identity is unchanged by this fix and it keeps serving without re-embedding.
	hosted := provider.Profile{Name: "gemini", Kind: provider.KindGemini,
		EmbedTextModel: "gemini-embedding-001", EmbedCodeModel: "gemini-embedding-001"}
	recorded := "gemini||gemini-embedding-001|gemini-embedding-001|0|0|off|off|off"
	if err := provider.VerifyEmbedIdentity(recorded, provider.EmbedIdentity(hosted, false, provider.EmbedContextualOff)); err != nil {
		t.Fatalf("a hosted-default gemini corpus must not be invalidated by #702: %v", err)
	}
}
