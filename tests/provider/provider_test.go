package tests

import (
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestMatrixMatchesSpec pins the normative SPEC 8.1.2 table exactly.
func TestMatrixMatchesSpec(t *testing.T) {
	S, E, U := provider.Supported, provider.EndpointDependent, provider.Unsupported
	want := map[provider.Kind]map[provider.Capability]provider.Support{
		provider.KindOpenAI:     {provider.CapEmbed: S, provider.CapChat: S, provider.CapOCR: U, provider.CapSTT: E, provider.CapTTS: E, provider.CapRerank: U},
		provider.KindMistral:    {provider.CapEmbed: U, provider.CapChat: U, provider.CapOCR: S, provider.CapSTT: S, provider.CapTTS: U, provider.CapRerank: U},
		provider.KindAnthropic:  {provider.CapEmbed: U, provider.CapChat: S, provider.CapOCR: U, provider.CapSTT: U, provider.CapTTS: U, provider.CapRerank: U},
		provider.KindGemini:     {provider.CapEmbed: S, provider.CapChat: S, provider.CapOCR: U, provider.CapSTT: S, provider.CapTTS: S, provider.CapRerank: U},
		provider.KindCohere:     {provider.CapEmbed: S, provider.CapChat: S, provider.CapOCR: U, provider.CapSTT: U, provider.CapTTS: U, provider.CapRerank: S},
		provider.KindElevenLabs: {provider.CapEmbed: U, provider.CapChat: U, provider.CapOCR: U, provider.CapSTT: S, provider.CapTTS: S, provider.CapRerank: U},
		provider.KindWhisper:    {provider.CapEmbed: U, provider.CapChat: U, provider.CapOCR: U, provider.CapSTT: S, provider.CapTTS: U, provider.CapRerank: U},
	}
	for k, caps := range want {
		for c, exp := range caps {
			if got := provider.Can(k, c); got != exp {
				t.Errorf("Can(%s,%s)=%d, want %d", k, c, got, exp)
			}
		}
	}
	if provider.Can("nope", provider.CapChat) != provider.Unsupported {
		t.Error("unknown kind must be Unsupported")
	}
	if provider.Can(provider.KindOpenAI, "nope") != provider.Unsupported {
		t.Error("unknown capability must be Unsupported")
	}
}

func TestEligible(t *testing.T) {
	if !(provider.Profile{APIKey: "k"}).Eligible() {
		t.Error("key present must be eligible")
	}
	if !(provider.Profile{CredentialLess: true}).Eligible() {
		t.Error("credential-less must be eligible")
	}
	if (provider.Profile{APIKey: "  "}).Eligible() {
		t.Error("blank key, not credential-less => not eligible")
	}
}

func cfgErr(t *testing.T, err error) *provider.ConfigError {
	t.Helper()
	var ce *provider.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *provider.ConfigError, got %v", err)
	}
	return ce
}

func TestSelectExplicit(t *testing.T) {
	openai := provider.Profile{Name: "openai", Kind: provider.KindOpenAI, APIKey: "k"}
	anth := provider.Profile{Name: "anthropic", Kind: provider.KindAnthropic, APIKey: "k"}
	noKey := provider.Profile{Name: "openai-nokey", Kind: provider.KindOpenAI}
	local := provider.Profile{Name: "local", Kind: provider.KindOpenAI, CredentialLess: true}
	byName := map[string]provider.Profile{
		"openai": openai, "anthropic": anth, "openai-nokey": noKey, "local": local,
	}

	// happy: capable + eligible
	if p, err := provider.Select(nil, byName, provider.CapChat, "openai", true); err != nil || p.Name != "openai" {
		t.Fatalf("explicit openai chat: %v %v", p, err)
	}
	// endpoint-dependent (openai STT) is NOT Unsupported -> allowed
	if _, err := provider.Select(nil, byName, provider.CapSTT, "openai", true); err != nil {
		t.Fatalf("explicit openai STT must be allowed (endpoint-dependent): %v", err)
	}
	// unknown profile
	if ce := cfgErr(t, mustErr(provider.Select(nil, byName, provider.CapChat, "ghost", true))); ce.Reason == "" {
		t.Error("want reason")
	}
	// kind cannot serve capability (anthropic embed)
	ce := cfgErr(t, mustErr(provider.Select(nil, byName, provider.CapEmbed, "anthropic", true)))
	if ce.Capability != provider.CapEmbed {
		t.Errorf("ce.Capability=%s", ce.Capability)
	}
	// required + ineligible (declares key, none present) -> ConfigError
	_ = cfgErr(t, mustErr(provider.Select(nil, byName, provider.CapChat, "openai-nokey", true)))
	// optional + ineligible explicit (rerank-capable cohere, no key) ->
	// ErrNoProvider (stays off, the rerank rule), NOT a hard
	// ConfigError (SPEC 8.1.3 case 1).
	byName["cohere-nokey"] = provider.Profile{Name: "cohere-nokey", Kind: provider.KindCohere}
	if err := mustErr(provider.Select(nil, byName, provider.CapRerank, "cohere-nokey", false)); !errors.Is(err, provider.ErrNoProvider) {
		t.Fatalf("optional+ineligible explicit should be ErrNoProvider, got %v", err)
	}
	// credential-less explicit is fine
	if _, err := provider.Select(nil, byName, provider.CapChat, "local", true); err != nil {
		t.Fatalf("credential-less explicit: %v", err)
	}
}

func TestSelectAutoPrecedence(t *testing.T) {
	mistral := provider.Profile{Name: "mistral", Kind: provider.KindMistral, APIKey: "k"} // no embed
	openai := provider.Profile{Name: "openai", Kind: provider.KindOpenAI, APIKey: "k"}    // embed yes
	cohere := provider.Profile{Name: "cohere", Kind: provider.KindCohere, APIKey: "k"}
	byName := map[string]provider.Profile{"mistral": mistral, "openai": openai, "cohere": cohere}

	// precedence [mistral, openai, cohere]: mistral can't embed -> openai wins
	p, err := provider.Select([]provider.Profile{mistral, openai, cohere}, byName, provider.CapEmbed, "", true)
	if err != nil || p.Name != "openai" {
		t.Fatalf("auto embed: got %v %v, want openai", p.Name, err)
	}
	// rerank: only cohere capable
	p, err = provider.Select([]provider.Profile{mistral, openai, cohere}, byName, provider.CapRerank, "", false)
	if err != nil || p.Name != "cohere" {
		t.Fatalf("auto rerank: got %v %v, want cohere", p.Name, err)
	}
	// ineligible skipped
	openaiNoKey := provider.Profile{Name: "openai", Kind: provider.KindOpenAI}
	p, err = provider.Select([]provider.Profile{openaiNoKey, cohere}, byName, provider.CapEmbed, "", false)
	if err != nil || p.Name != "cohere" {
		t.Fatalf("ineligible skipped: got %v %v, want cohere", p.Name, err)
	}
	// none capable+eligible -> ErrNoProvider
	if _, err := provider.Select([]provider.Profile{mistral}, byName, provider.CapRerank, "", false); !errors.Is(err, provider.ErrNoProvider) {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
	// credential-less local auto-selected
	local := provider.Profile{Name: "local", Kind: provider.KindOpenAI, CredentialLess: true}
	p, err = provider.Select([]provider.Profile{local}, byName, provider.CapChat, "", true)
	if err != nil || p.Name != "local" {
		t.Fatalf("credential-less auto: got %v %v", p.Name, err)
	}
}

func TestEmbedIdentity(t *testing.T) {
	p := provider.Profile{Name: "mistral", EmbedTextModel: "mistral-embed", EmbedCodeModel: "codestral-embed"}
	id := provider.EmbedIdentity(p, false)
	// Identity is provider|base_url|text_model|code_model|text_dim|code_dim|
	// multimodal|late_chunking (SPEC 8.1.4/8.1.6/8.1.7, issue #332/#446/#560);
	// an unset/canonical base_url records as "", unset dims as 0, multimodal +
	// late_chunking as off.
	if id != "mistral||mistral-embed|codestral-embed|0|0|off|off" {
		t.Fatalf("identity = %q", id)
	}
	if err := provider.VerifyEmbedIdentity("", id); err != nil {
		t.Errorf("fresh index (empty recorded) must pass: %v", err)
	}
	if err := provider.VerifyEmbedIdentity(id, id); err != nil {
		t.Errorf("matching identity must pass: %v", err)
	}
	_ = cfgErr(t, provider.VerifyEmbedIdentity("openai|text-embedding-3-small||0|0|off", id))
	// A different requested output dimension is a distinct identity
	// (reindex-bound, SPEC 8.1.6): same provider+models but dim 768 must
	// not match the native (dim 0) identity.
	native := provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-001"}, false)
	dimmed := provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-001", EmbedTextDim: 768}, false)
	if native == dimmed {
		t.Fatalf("requested dimension must change embed identity: %q == %q", native, dimmed)
	}
	// A different multimodal mode is a distinct identity (reindex-bound,
	// SPEC 8.1.7).
	mm := provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-2", EmbedCodeModel: "gemini-embedding-2", EmbedMultimodal: "augment"}, false)
	if mm == provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-2", EmbedCodeModel: "gemini-embedding-2"}, false) {
		t.Fatalf("multimodal mode must change embed identity")
	}
	// A different late-chunking mode is a distinct identity (reindex-bound,
	// issue #332/#446): the same profile with the mode on must NOT match off.
	if provider.EmbedIdentity(p, true) == provider.EmbedIdentity(p, false) {
		t.Fatalf("late-chunking mode must change embed identity")
	}
	// Legacy identities must NOT force a spurious reindex against the
	// equivalent current identity. Every pre-base_url form (issue #560) gains
	// an EMPTY base_url component at position 2, so an existing hosted-default
	// corpus (whose current base_url normalizes to "") stays valid: a 3-field
	// (pre-8.1.6) normalizes to "||…|0|0|off|off", a 5-field (pre-8.1.7) to
	// "||…|off|off", a 6-field (pre-late-chunking, #446) to "||…|off", and a
	// 7-field (pre-base_url, #560) gains only the empty base_url. The current
	// side is the 8-field form.
	current := "mistral||mistral-embed|codestral-embed|0|0|off|off"
	legacy3 := "mistral|mistral-embed|codestral-embed"
	if err := provider.VerifyEmbedIdentity(legacy3, current); err != nil {
		t.Errorf("legacy 3-field identity must match native current: %v", err)
	}
	legacy5 := "mistral|mistral-embed|codestral-embed|0|0"
	if err := provider.VerifyEmbedIdentity(legacy5, current); err != nil {
		t.Errorf("legacy 5-field identity must match off-mode current: %v", err)
	}
	legacy6 := "mistral|mistral-embed|codestral-embed|0|0|off"
	if err := provider.VerifyEmbedIdentity(legacy6, current); err != nil {
		t.Errorf("legacy 6-field identity must match late-chunking-off current: %v", err)
	}
	// pre-base_url 7-field form (had the late-chunking field, no base_url).
	legacy7 := "mistral|mistral-embed|codestral-embed|0|0|off|off"
	if err := provider.VerifyEmbedIdentity(legacy7, current); err != nil {
		t.Errorf("legacy 7-field (pre-base_url) identity must match current: %v", err)
	}
	// But a legacy identity vs a non-native dimension still mismatches.
	_ = cfgErr(t, provider.VerifyEmbedIdentity(legacy3, "mistral||mistral-embed|codestral-embed|768|0|off|off"))
}

// TestEmbedIdentity_BaseURL exercises the SPEC 8.1.4 base_url component of the
// embed identity (issue #560/#440 F3): two same-kind/same-model profiles at
// DIFFERENT endpoints must NOT collapse to one identity, while a canonical /
// default / native-surface endpoint normalizes to "" so no hosted-default
// corpus spuriously reindexes.
func TestEmbedIdentity_BaseURL(t *testing.T) {
	// baseURLOf extracts the 2nd identity field (the normalized base_url).
	baseURLOf := func(p provider.Profile) string {
		parts := strings.Split(provider.EmbedIdentity(p, false), "|")
		if len(parts) < 2 {
			t.Fatalf("identity has no base_url field: %q", provider.EmbedIdentity(p, false))
		}
		return parts[1]
	}

	// Rule 2 — canonical/default → "": a built-in profile AT ITS OWN shipped
	// endpoint (matched by profile name), plus an unset base_url, normalize to
	// "". The name must be the built-in's name: the collapse is per-profile, not
	// kind-wide (see TestEmbedIdentity_PerProfileCanonical).
	for _, tc := range []struct{ name, base string }{
		{"openai", ""},                          // unset → client wire default
		{"openai", "https://api.openai.com/v1"}, // explicit wire default
		{"mistral", "https://api.mistral.ai/v1"},
		{"openrouter", "https://openrouter.ai/api/v1"},
		{"local", "http://localhost:11434/v1"},
	} {
		p := provider.Profile{Name: tc.name, Kind: provider.KindOpenAI, BaseURL: tc.base,
			EmbedTextModel: "text-embedding-3-small"}
		if got := baseURLOf(p); got != "" {
			t.Errorf("%s @ %q: a built-in at its own default must normalize to \"\", got %q", tc.name, tc.base, got)
		}
	}

	// Rule 1 — not meaningful: native gemini/cohere normalize to "" regardless
	// of any configured base_url (single canonical provider surface).
	for _, k := range []provider.Kind{provider.KindGemini, provider.KindCohere} {
		p := provider.Profile{Name: string(k), Kind: k, BaseURL: "https://custom.example.com/v9",
			EmbedTextModel: "m"}
		if got := baseURLOf(p); got != "" {
			t.Errorf("kind %s: base_url must not participate (rule 1), got %q", k, got)
		}
	}

	// A custom (non-canonical) endpoint yields a non-empty component, and two
	// kind:openai profiles at DIFFERENT custom endpoints produce DIFFERENT
	// identities (the #560 bug: without base_url they collapse and mix vectors).
	a := provider.Profile{Name: "vllm", Kind: provider.KindOpenAI, CredentialLess: true,
		BaseURL: "https://vllm-a.internal/v1", EmbedTextModel: "bge-m3"}
	b := provider.Profile{Name: "vllm", Kind: provider.KindOpenAI, CredentialLess: true,
		BaseURL: "https://vllm-b.internal/v1", EmbedTextModel: "bge-m3"}
	if baseURLOf(a) == "" || baseURLOf(b) == "" {
		t.Fatalf("custom endpoints must yield a non-empty base_url component: %q %q", baseURLOf(a), baseURLOf(b))
	}
	idA, idB := provider.EmbedIdentity(a, false), provider.EmbedIdentity(b, false)
	if idA == idB {
		t.Fatalf("two kind:openai profiles at different custom endpoints must differ: %q", idA)
	}
	// A corpus built on custom endpoint A must refuse to serve on endpoint B.
	_ = cfgErr(t, provider.VerifyEmbedIdentity(idA, idB))
	// …and match itself (no spurious reindex on the same custom endpoint).
	if err := provider.VerifyEmbedIdentity(idA, idA); err != nil {
		t.Errorf("same custom endpoint must pass: %v", err)
	}

	// Rule 3 — URL canonicalization: trailing slash, default port, uppercase
	// host, userinfo, and duplicate slashes all normalize to one value.
	canon := baseURLOf(a) // "https://vllm-a.internal/v1"
	for _, variant := range []string{
		"https://vllm-a.internal/v1/",
		"https://vllm-a.internal:443/v1",
		"https://VLLM-A.INTERNAL/v1",
		"https://user:secret@vllm-a.internal/v1", // userinfo dropped (never recorded/logged)
		"https://vllm-a.internal//v1",
		"https://vllm-a.internal/v1?x=1#frag",
	} {
		p := provider.Profile{Name: "vllm", Kind: provider.KindOpenAI, CredentialLess: true,
			BaseURL: variant, EmbedTextModel: "bge-m3"}
		if got := baseURLOf(p); got != canon {
			t.Errorf("canonicalization: %q normalized to %q, want %q", variant, got, canon)
		}
	}

	// A pre-base_url legacy identity (recorded on a hosted default, no base_url)
	// stays valid against the current hosted-default identity — no spurious
	// reindex — but mismatches a corpus pinned to a custom endpoint.
	hostedCurrent := provider.EmbedIdentity(provider.Profile{Name: "mistral", Kind: provider.KindOpenAI,
		BaseURL: "https://api.mistral.ai/v1", EmbedTextModel: "mistral-embed", EmbedCodeModel: "codestral-embed"}, false)
	legacyHosted := "mistral|mistral-embed|codestral-embed|0|0|off|off" // pre-base_url 7-field
	if err := provider.VerifyEmbedIdentity(legacyHosted, hostedCurrent); err != nil {
		t.Errorf("pre-base_url hosted-default identity must not reindex: %v", err)
	}
}

// TestEmbedIdentity_PerProfileCanonical is the #560 / CodeRabbit regression: the
// Rule-2 collapse to "" is PER-PROFILE, not kind-wide. A built-in `mistral`
// profile pointed at a *different* listed vendor's host (api.openai.com) is a
// non-canonical override — it must NOT collapse to "" and must produce a
// DIFFERENT identity from the same-named, same-model profile at its real
// default, or vectors from two backends could silently share one index.
func TestEmbedIdentity_PerProfileCanonical(t *testing.T) {
	baseURLOf := func(p provider.Profile) string {
		return strings.Split(provider.EmbedIdentity(p, false), "|")[1]
	}
	mistralReal := provider.Profile{Name: "mistral", Kind: provider.KindOpenAI,
		BaseURL: "https://api.mistral.ai/v1", EmbedTextModel: "text-embedding-3-small"}
	mistralAtOpenAI := provider.Profile{Name: "mistral", Kind: provider.KindOpenAI,
		BaseURL: "https://api.openai.com/v1", EmbedTextModel: "text-embedding-3-small"}
	if got := baseURLOf(mistralAtOpenAI); got == "" {
		t.Errorf("a mistral profile pointed at api.openai.com must NOT collapse to \"\" (kind-wide mixing bug)")
	}
	if provider.EmbedIdentity(mistralReal, false) == provider.EmbedIdentity(mistralAtOpenAI, false) {
		t.Errorf("mistral@mistral and mistral@openai must have distinct identities")
	}
}

// TestEmbedBaseURL_CanonicalizationRobustness pins the CodeRabbit review fixes on
// canonicalizeEmbedBaseURL: an IPv6 literal stays bracketed, a percent-encoded
// slash is preserved (not folded into a path separator), and a value that does
// not parse as an absolute URL can never leak the "|" identity delimiter, a
// control character, or a userinfo credential into the recorded component.
func TestEmbedBaseURL_CanonicalizationRobustness(t *testing.T) {
	// A non-built-in profile name records the canonicalized base_url verbatim
	// (no Rule-2 collapse), so we observe canonicalizeEmbedBaseURL directly.
	norm := func(base string) string {
		return provider.NormalizeEmbedBaseURL(provider.Profile{
			Name: "custom", Kind: provider.KindOpenAI, CredentialLess: true, BaseURL: base})
	}

	// IPv6 literal: brackets preserved, default port dropped, non-default kept.
	if got := norm("https://[2001:db8::1]:443/v1"); got != "https://[2001:db8::1]/v1" {
		t.Errorf("IPv6 default-port: got %q", got)
	}
	if got := norm("https://[2001:db8::1]:8443/v1"); got != "https://[2001:db8::1]:8443/v1" {
		t.Errorf("IPv6 custom-port: got %q", got)
	}

	// Percent-encoded slash is a distinct path byte, not a separator: it must
	// survive canonicalization (else /a%2Fb and /a/b would collapse to one id).
	if got := norm("https://h.internal/a%2Fb/v1"); !strings.Contains(got, "%2F") && !strings.Contains(got, "%2f") {
		t.Errorf("encoded slash must be preserved, got %q", got)
	}

	// Fallback (unparseable/relative): never emit "|" (would corrupt the
	// 8-field identity), control chars, or a userinfo credential.
	for _, base := range []string{"${EMBED_URL}|inject", "user:secret@${EMBED_URL}", "raw\x01ctl"} {
		got := norm(base)
		if strings.Contains(got, "|") {
			t.Errorf("fallback must strip the | delimiter: %q → %q", base, got)
		}
		if strings.ContainsAny(got, "\x00\x01\x1f\x7f") {
			t.Errorf("fallback must strip control chars: %q → %q", base, got)
		}
	}
	if got := norm("user:secret@${EMBED_URL}"); strings.Contains(got, "secret") {
		t.Errorf("fallback must drop a userinfo credential: got %q", got)
	}
	// The delimiter guarantee holds end-to-end: the identity keeps exactly 8 fields.
	id := provider.EmbedIdentity(provider.Profile{Name: "custom", Kind: provider.KindOpenAI,
		CredentialLess: true, BaseURL: "${EMBED_URL}|x", EmbedTextModel: "m"}, false)
	if n := strings.Count(id, "|"); n != 7 {
		t.Errorf("identity must have 8 fields (7 pipes), got %d: %q", n, id)
	}
}

func mustErr(_ provider.Profile, err error) error {
	return err
}

// TestIsKnownKind pins issue #440 F7's building block: every kind in the
// capability matrix is recognized, and a typo is not. An unrecognized kind is
// what the config layer rejects as CONFIG_INVALID at startup.
func TestIsKnownKind(t *testing.T) {
	for _, k := range []provider.Kind{
		provider.KindOpenAI, provider.KindMistral, provider.KindAnthropic,
		provider.KindGemini, provider.KindCohere, provider.KindElevenLabs,
		provider.KindWhisper, provider.KindOmniEmbed, provider.KindColBERT,
	} {
		if !provider.IsKnownKind(k) {
			t.Errorf("IsKnownKind(%q) = false, want true", k)
		}
	}
	for _, k := range []provider.Kind{"", "opnai", "gemni", "OpenAI"} {
		if provider.IsKnownKind(k) {
			t.Errorf("IsKnownKind(%q) = true, want false", k)
		}
	}
	if provider.KnownKindsString() == "" {
		t.Fatal("KnownKindsString must list the recognized kinds for remediation")
	}
}
