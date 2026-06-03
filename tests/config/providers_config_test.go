package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

func loadCfg(t *testing.T, yaml string) config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, p, yaml)
	cfg, err := config.LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return cfg
}

func TestProviders_BuiltinAutoSelectByCredential(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")
	cfg := loadCfg(t, "rag:\n  generate_answer: true\n")
	r := cfg.Providers()

	embed, err := r.Resolve(provider.CapEmbed)
	if err != nil || embed.Name != "mistral" || embed.Kind != provider.KindOpenAI {
		t.Fatalf("embed auto = %+v, %v; want builtin mistral(openai)", embed, err)
	}
	if embed.EmbedTextModel != "mistral-embed" {
		t.Fatalf("embed text model = %q", embed.EmbedTextModel)
	}
	ocr, err := r.Resolve(provider.CapOCR)
	if err != nil || ocr.Kind != provider.KindMistral {
		t.Fatalf("ocr auto = %+v, %v; want mistral-ocr kind=mistral", ocr, err)
	}
	// rerank optional: no COHERE key -> ErrNoProvider (stays off)
	if _, err := r.Resolve(provider.CapRerank); err == nil {
		t.Fatal("rerank without COHERE_API_KEY must not resolve")
	}
}

func TestProviders_NoCredentialEmbedFails(t *testing.T) {
	// Deterministic: blank any embed-capable provider key the host/CI
	// may already export (Resolve reads process env via cfg.Providers).
	for _, k := range []string{
		"MISTRAL_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
		"GEMINI_API_KEY", "COHERE_API_KEY", "ANTHROPIC_API_KEY", "ELEVENLABS_API_KEY",
	} {
		t.Setenv(k, "")
	}
	// No creds and no explicit binding: auto-select must NOT silently
	// fall through to the credential-less `local` (it's excluded from
	// auto precedence) — embed must fail so preflight surfaces it.
	cfg := loadCfg(t, "version: 1\n")
	if _, err := cfg.Providers().Resolve(provider.CapEmbed); err == nil {
		t.Fatal("embed must not resolve with no credentials and no explicit binding")
	}
	// But an explicit `local` binding works (credential-less, eligible).
	exp := loadCfg(t, "model:\n  embed:\n    provider: local\n")
	p, err := exp.Providers().Resolve(provider.CapEmbed)
	if err != nil || p.Name != "local" {
		t.Fatalf("explicit local embed should resolve, got %+v %v", p, err)
	}
}

func TestProviders_ExplicitBindingAndMatrixValidation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	t.Setenv("MISTRAL_API_KEY", "mk")
	cfg := loadCfg(t, "model:\n  chat:\n    provider: anthropic\n")
	r := cfg.Providers()

	chat, err := r.Resolve(provider.CapChat)
	if err != nil || chat.Name != "anthropic" {
		t.Fatalf("explicit chat=anthropic: %+v %v", chat, err)
	}
	// anthropic cannot embed -> binding it would be CONFIG_INVALID
	bad := loadCfg(t, "model:\n  embed:\n    provider: anthropic\n")
	if _, err := bad.Providers().Resolve(provider.CapEmbed); err == nil {
		t.Fatal("anthropic embed binding must be CONFIG_INVALID")
	}
}

func TestProviders_UserOverrideAndCustomProfile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ok")
	yaml := "" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://proxy.example/v1\n" +
		"    chat_model: gpt-custom\n" +
		"  groq:\n" +
		"    kind: openai\n" +
		"    base_url: https://api.groq.com/openai/v1\n" +
		"    api_key: ${OPENAI_API_KEY}\n" +
		"model:\n" +
		"  chat:\n" +
		"    provider: openai\n"
	r := loadCfg(t, yaml).Providers()

	// per-field override of a built-in
	chat, err := r.Resolve(provider.CapChat)
	if err != nil || chat.Name != "openai" {
		t.Fatalf("chat: %+v %v", chat, err)
	}
	if chat.BaseURL != "https://proxy.example/v1" || chat.ChatModel != "gpt-custom" {
		t.Fatalf("user override not applied: base=%q chat=%q", chat.BaseURL, chat.ChatModel)
	}

	// the user-only `groq` profile must be registered AND its
	// ${OPENAI_API_KEY} must have expanded (env-sourced credential).
	gp, err := provider.Select(nil, r.ByName(), provider.CapChat, "groq", true)
	if err != nil {
		t.Fatalf("custom groq profile not registered: %v", err)
	}
	if gp.BaseURL != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq base_url = %q", gp.BaseURL)
	}
	if gp.APIKey != "ok" {
		t.Fatalf("groq ${OPENAI_API_KEY} not expanded: APIKey=%q want %q", gp.APIKey, "ok")
	}
}

func TestProviders_EmbedIdentityStable(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")
	id := loadCfg(t, "version: 1\n").Providers().EmbedIdentity()
	// provider|text_model|code_model|text_dim|code_dim|multimodal
	// (SPEC 8.1.4/8.1.6/8.1.7); default Mistral: native dims, mode off.
	if id != "mistral|mistral-embed|codestral-embed|0|0|off" {
		t.Fatalf("embed identity = %q", id)
	}
}

// TestProviders_EmbedDimensionKnob pins SPEC 8.1.6: model.embed.text_dim/
// code_dim parse, resolve onto the embed profile, and enter the embed
// identity (so a dimension change is reindex-bound).
func TestProviders_EmbedDimensionKnob(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	yaml := "version: 1\n" +
		"model:\n" +
		"  embed:\n" +
		"    provider: gemini\n" +
		"    text_dim: 1536\n" +
		"    code_dim: 768\n"
	r := loadCfg(t, yaml).Providers()
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	if p.Name != "gemini" {
		t.Fatalf("embed provider = %q, want gemini", p.Name)
	}
	if p.EmbedTextDim != 1536 || p.EmbedCodeDim != 768 {
		t.Fatalf("dims = text:%d code:%d, want 1536/768", p.EmbedTextDim, p.EmbedCodeDim)
	}
	id := r.EmbedIdentity()
	if !strings.HasSuffix(id, "|1536|768|off") {
		t.Fatalf("embed identity %q must encode requested dims (and off mode)", id)
	}
}

// TestProviders_EmbedMultimodalKnob pins SPEC 8.1.7: model.embed.multimodal
// parses, resolves onto the embed profile, and enters the embed identity.
func TestProviders_EmbedMultimodalKnob(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	yaml := "version: 1\n" +
		"model:\n" +
		"  embed:\n" +
		"    provider: gemini\n" +
		"    text_model: gemini-embedding-2\n" +
		"    code_model: gemini-embedding-2\n" +
		"    multimodal: augment\n"
	r := loadCfg(t, yaml).Providers()
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed: %v", err)
	}
	if p.EmbedMultimodal != "augment" {
		t.Fatalf("multimodal = %q, want augment", p.EmbedMultimodal)
	}
	if !strings.HasSuffix(r.EmbedIdentity(), "|augment") {
		t.Fatalf("embed identity %q must encode the multimodal mode", r.EmbedIdentity())
	}
}

func TestProviders_EnvVarRefs(t *testing.T) {
	// Default config (no custom providers): built-in profiles reference
	// the standard credential env vars.
	refs := loadCfg(t, "version: 1\n").ProviderEnvVarRefs()
	inRefs := func(key string) bool {
		for _, r := range refs {
			if r == key {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"MISTRAL_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if !inRefs(want) {
			t.Errorf("ProviderEnvVarRefs missing built-in key %q; got %v", want, refs)
		}
	}
	// A custom provider with ${MY_CUSTOM_KEY} must also appear.
	custom := loadCfg(t, "providers:\n  myprovider:\n    kind: openai\n    api_key: ${MY_CUSTOM_KEY}\n")
	customRefs := custom.ProviderEnvVarRefs()
	if !func() bool {
		for _, r := range customRefs {
			if r == "MY_CUSTOM_KEY" {
				return true
			}
		}
		return false
	}() {
		t.Errorf("ProviderEnvVarRefs missing custom key MY_CUSTOM_KEY; got %v", customRefs)
	}
}
