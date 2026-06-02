package tests

import (
	"errors"
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
	id := provider.EmbedIdentity(p)
	// Identity is provider|text_model|code_model|text_dim|code_dim
	// (SPEC 8.1.4/8.1.6); unset dims record as 0 (native).
	if id != "mistral|mistral-embed|codestral-embed|0|0" {
		t.Fatalf("identity = %q", id)
	}
	if err := provider.VerifyEmbedIdentity("", id); err != nil {
		t.Errorf("fresh index (empty recorded) must pass: %v", err)
	}
	if err := provider.VerifyEmbedIdentity(id, id); err != nil {
		t.Errorf("matching identity must pass: %v", err)
	}
	_ = cfgErr(t, provider.VerifyEmbedIdentity("openai|text-embedding-3-small||0|0", id))
	// A different requested output dimension is a distinct identity
	// (reindex-bound, SPEC 8.1.6): same provider+models but dim 768 must
	// not match the native (dim 0) identity.
	native := provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-001"})
	dimmed := provider.EmbedIdentity(provider.Profile{Name: "gemini", EmbedTextModel: "gemini-embedding-001", EmbedTextDim: 768})
	if native == dimmed {
		t.Fatalf("requested dimension must change embed identity: %q == %q", native, dimmed)
	}
	// A legacy 3-field identity (pre-8.1.6 snapshot, no dims) must NOT
	// force a spurious reindex against the equivalent native 5-field
	// identity — it is normalized to "|0|0" before comparison.
	legacy := "mistral|mistral-embed|codestral-embed"
	if err := provider.VerifyEmbedIdentity(legacy, "mistral|mistral-embed|codestral-embed|0|0"); err != nil {
		t.Errorf("legacy 3-field identity must match native 5-field: %v", err)
	}
	// But a legacy identity vs a non-native dimension still mismatches.
	_ = cfgErr(t, provider.VerifyEmbedIdentity(legacy, "mistral|mistral-embed|codestral-embed|768|0"))
}

func mustErr(_ provider.Profile, err error) error {
	return err
}
