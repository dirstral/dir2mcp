package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestSTTAuto_ResolvesVoxtralNotOpenAICompat pins issue #440 F4: with
// stt_provider=auto and only MISTRAL_API_KEY present, STT auto-selection must
// resolve the Voxtral-backed `mistral-ocr` profile (kind: mistral, statically
// STT-Supported), NOT the OpenAI-compatible `mistral` chat/embed profile
// (kind: openai, STT only EndpointDependent). Before the precedence reorder,
// `mistral` sat ahead of `mistral-ocr` and shadowed it, binding the wrong
// transcriber and never using the seeded Voxtral model.
func TestSTTAuto_ResolvesVoxtralNotOpenAICompat(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "k")
	t.Setenv("ELEVENLABS_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	cfg := config.Default()
	cfg.STTProvider = "auto"

	name, model, ok := ingest.ResolveSTTProviderModel(cfg)
	if !ok {
		t.Fatal("expected an STT provider to resolve in auto mode with MISTRAL_API_KEY set")
	}
	if name != "mistral-ocr" {
		t.Fatalf("auto STT resolved provider = %q, want mistral-ocr (Voxtral), not the openai-compat mistral profile", name)
	}
	if model != "voxtral-mini-latest" {
		t.Fatalf("auto STT resolved model = %q, want voxtral-mini-latest", model)
	}
}

// TestSTTExplicit_ProviderReportedTruthfully pins issue #440 F5: an explicitly
// configured non-Mistral STT backend must be reported by its own resolved profile
// name, not the hardcoded "mistral". Exercised via the same resolver the transcribe
// tool result now uses for its provenance.
func TestSTTExplicit_ProviderReportedTruthfully(t *testing.T) {
	t.Setenv("ELEVENLABS_API_KEY", "ek")
	cfg := config.Default()
	cfg.STTProvider = "elevenlabs"

	name, _, ok := ingest.ResolveSTTProviderModel(cfg)
	if !ok {
		t.Fatal("expected the explicit elevenlabs STT provider to resolve")
	}
	if name != "elevenlabs" {
		t.Fatalf("explicit STT provider reported as %q, want elevenlabs (not hardcoded mistral)", name)
	}
}
