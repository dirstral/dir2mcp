package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// These tests pin the SPEC §8.5 self-hosted / OpenAI-compatible endpoint
// contract (dir2mcp#240): a provider profile that carries only a custom
// base_url (the GPU-VPS host) resolves end-to-end for each capability —
// embed, OCR, and STT — without depending on a hosted SaaS credential.

// TestSelfHosted_EmbedBaseURLResolves: a credential-less kind:openai profile
// on a self-hosted base_url drives CapEmbed when bound via model.embed.provider
// (SPEC §8.5 embed → POST /v1/embeddings). Self-hosted endpoints may be
// credential-less and must still be eligible (§8.1.1/§8.1.3).
func TestSelfHosted_EmbedBaseURLResolves(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  gpu-embed:\n" +
		"    kind: openai\n" +
		"    base_url: http://gpu-vps:8080/v1\n" +
		"    embed_text_model: bge-m3\n" +
		"model:\n" +
		"  embed:\n" +
		"    provider: gpu-embed\n"
	r := loadCfg(t, yaml).Providers()

	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("resolve embed on self-hosted base_url: %v", err)
	}
	if p.Name != "gpu-embed" {
		t.Fatalf("embed provider = %q, want gpu-embed", p.Name)
	}
	if p.Kind != provider.KindOpenAI {
		t.Fatalf("embed kind = %q, want openai", p.Kind)
	}
	if !p.CredentialLess || !p.Eligible() {
		t.Fatalf("credential-less self-hosted embed profile must be eligible: credLess=%v eligible=%v",
			p.CredentialLess, p.Eligible())
	}
	if p.BaseURL != "http://gpu-vps:8080/v1" {
		t.Fatalf("embed base_url = %q", p.BaseURL)
	}
	if p.EmbedTextModel != "bge-m3" {
		t.Fatalf("embed text model = %q, want bge-m3", p.EmbedTextModel)
	}
	// The embed identity must encode the self-hosted profile + custom base_url
	// + model so a change to any is corpus-lifetime / reindex-bound (SPEC
	// §8.1.4 / §8.5 / issue #560): the non-canonical endpoint is the 2nd field.
	if id := r.EmbedIdentity(); id != "gpu-embed|http://gpu-vps:8080/v1|bge-m3||0|0|off|off|off" {
		t.Fatalf("embed identity = %q", id)
	}
}

// TestSelfHosted_OCRBaseURLResolves: a bespoke-OCR profile (kind:mistral, the
// /v1/ocr surface) on a self-hosted base_url resolves for CapOCR when bound via
// model.ocr.provider. Per SPEC §8.5 OCR has no OpenAI analog and stays a
// bespoke surface, but its base_url is still operator-pointable at a GPU host.
func TestSelfHosted_OCRBaseURLResolves(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  gpu-ocr:\n" +
		"    kind: mistral\n" +
		"    base_url: http://gpu-vps:9000/v1\n" +
		"    ocr_model: my-ocr\n" +
		"model:\n" +
		"  ocr:\n" +
		"    provider: gpu-ocr\n"
	r := loadCfg(t, yaml).Providers()

	// The explicit model.ocr.provider binding is what ingest reads.
	if name := r.OCRProviderName(); name != "gpu-ocr" {
		t.Fatalf("OCRProviderName = %q, want gpu-ocr", name)
	}
	p, err := r.Resolve(provider.CapOCR)
	if err != nil {
		t.Fatalf("resolve ocr on self-hosted base_url: %v", err)
	}
	if p.Name != "gpu-ocr" || p.Kind != provider.KindMistral {
		t.Fatalf("ocr provider = %+v; want gpu-ocr kind=mistral", p)
	}
	if p.BaseURL != "http://gpu-vps:9000/v1" {
		t.Fatalf("ocr base_url = %q", p.BaseURL)
	}
	if p.OCRModel != "my-ocr" {
		t.Fatalf("ocr model = %q, want my-ocr", p.OCRModel)
	}
	if !p.CredentialLess || !p.Eligible() {
		t.Fatalf("credential-less self-hosted ocr profile must be eligible: credLess=%v eligible=%v",
			p.CredentialLess, p.Eligible())
	}
}

// TestSelfHosted_OCRBaseURLRejectedForOpenAIKind pins SPEC §8.5: OCR is NOT
// reachable through the OpenAI-compatible contract. Binding model.ocr.provider
// to a kind:openai self-hosted profile is CONFIG_INVALID (matrix: ocr ❌ for
// openai), so an operator is steered to docling-serve or a kind:mistral OCR
// surface instead of silently producing no OCR.
func TestSelfHosted_OCRBaseURLRejectedForOpenAIKind(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  gpu-openai:\n" +
		"    kind: openai\n" +
		"    base_url: http://gpu-vps:8080/v1\n" +
		"model:\n" +
		"  ocr:\n" +
		"    provider: gpu-openai\n"
	r := loadCfg(t, yaml).Providers()
	if _, err := r.Resolve(provider.CapOCR); err == nil {
		t.Fatal("ocr bound to kind:openai must be CONFIG_INVALID (SPEC §8.5: no OpenAI OCR analog)")
	}
}

// TestSelfHosted_STTBaseURLResolves: the self-hosted whisper profile resolves
// for CapSTT once its base_url is set (SPEC §8.5 stt → POST
// /v1/audio/transcriptions). It is credential-less and eligible without a key.
func TestSelfHosted_STTBaseURLResolves(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  whisper:\n" +
		"    base_url: http://gpu-vps:9001/v1\n" +
		"    stt_model: large-v3\n"
	r := loadCfg(t, yaml).Providers()

	p, err := r.ResolveExplicit(provider.CapSTT, "whisper", true)
	if err != nil {
		t.Fatalf("resolve stt on self-hosted whisper base_url: %v", err)
	}
	if p.Name != "whisper" || p.Kind != provider.KindWhisper {
		t.Fatalf("stt provider = %+v; want whisper kind=whisper", p)
	}
	if p.BaseURL != "http://gpu-vps:9001/v1" {
		t.Fatalf("stt base_url = %q", p.BaseURL)
	}
	if p.STTModel != "large-v3" {
		t.Fatalf("stt model = %q, want large-v3", p.STTModel)
	}
	if !p.CredentialLess || !p.Eligible() {
		t.Fatalf("credential-less self-hosted stt profile must be eligible: credLess=%v eligible=%v",
			p.CredentialLess, p.Eligible())
	}
}

// TestSelfHosted_STTOpenAIKindBaseURLResolves: a generic kind:openai
// self-hosted endpoint also serves STT (endpoint-dependent, validated at first
// use per SPEC §8.1.2 ³ / §8.5), so binding it for STT must resolve.
func TestSelfHosted_STTOpenAIKindBaseURLResolves(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  gpu-stt:\n" +
		"    kind: openai\n" +
		"    base_url: http://gpu-vps:8080/v1\n" +
		"    stt_model: whisper-1\n"
	r := loadCfg(t, yaml).Providers()
	p, err := r.ResolveExplicit(provider.CapSTT, "gpu-stt", true)
	if err != nil {
		t.Fatalf("resolve stt on self-hosted kind:openai base_url: %v", err)
	}
	if p.Name != "gpu-stt" || p.Kind != provider.KindOpenAI {
		t.Fatalf("stt provider = %+v; want gpu-stt kind=openai", p)
	}
	if p.BaseURL != "http://gpu-vps:8080/v1" {
		t.Fatalf("stt base_url = %q", p.BaseURL)
	}
}
