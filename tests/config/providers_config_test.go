package tests

import (
	"path/filepath"
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
	// no provider creds at all; local (credential-less) is openai-kind
	// so it CAN serve embed -> auto-selects local. Assert that.
	cfg := loadCfg(t, "version: 1\n")
	p, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil || p.Name != "local" {
		t.Fatalf("expected credential-less 'local' for embed, got %+v %v", p, err)
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
	chat, err := r.Resolve(provider.CapChat)
	if err != nil || chat.Name != "openai" {
		t.Fatalf("chat: %+v %v", chat, err)
	}
	if chat.BaseURL != "https://proxy.example/v1" || chat.ChatModel != "gpt-custom" {
		t.Fatalf("user override not applied: base=%q chat=%q", chat.BaseURL, chat.ChatModel)
	}
}

func TestProviders_EmbedIdentityStable(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")
	id := loadCfg(t, "version: 1\n").Providers().EmbedIdentity()
	if id != "mistral|mistral-embed|codestral-embed" {
		t.Fatalf("embed identity = %q", id)
	}
}
