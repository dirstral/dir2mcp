package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestSTTExplicit_GeminiResolvesProfile pins issue #440 F6: an explicit
// stt_provider=gemini must resolve the built-in `gemini` profile (which
// providerfactory.Transcriber supports) instead of silently resolving STT-off.
// Before unifying the selector tables, `gemini`/`openai` were mapped by NEITHER
// resolveSTTProfile NOR the diarization-gating resolver, so the provider was
// reported as absent while transcription would have errored loudly at first use.
func TestSTTExplicit_GeminiResolvesProfile(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	cfg := config.Default()
	cfg.STTProvider = "gemini"

	name, _, ok := ingest.ResolveSTTProviderModel(cfg)
	if !ok {
		t.Fatal("expected explicit stt_provider=gemini to resolve an STT profile, not silently resolve STT-off")
	}
	if name != "gemini" {
		t.Fatalf("explicit gemini STT resolved profile = %q, want gemini", name)
	}

	tr, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("build gemini transcriber: unexpected error %v", err)
	}
	if tr == nil {
		t.Fatal("expected a non-nil gemini transcriber for an explicit stt_provider=gemini selector")
	}
}

// TestSTTExplicit_OpenAIResolvesProfile pins issue #440 F6 for the OpenAI STT
// selector: kind:openai STT is EndpointDependent (validated at first use), but
// an explicit selector must still resolve the `openai` profile and build a
// transcriber rather than resolve STT-off.
func TestSTTExplicit_OpenAIResolvesProfile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ok")
	cfg := config.Default()
	cfg.STTProvider = "openai"

	name, _, ok := ingest.ResolveSTTProviderModel(cfg)
	if !ok {
		t.Fatal("expected explicit stt_provider=openai to resolve an STT profile, not silently resolve STT-off")
	}
	if name != "openai" {
		t.Fatalf("explicit openai STT resolved profile = %q, want openai", name)
	}

	tr, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("build openai transcriber: unexpected error %v", err)
	}
	if tr == nil {
		t.Fatal("expected a non-nil openai transcriber for an explicit stt_provider=openai selector")
	}
}

// TestSTTSelector_UnknownFailsValidateFast pins issue #440 F6: an unknown /
// unmappable stt.provider selector must fail fast at startup with CONFIG_INVALID
// rather than silently disabling STT on the resolver paths.
func TestSTTSelector_UnknownFailsValidateFast(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "definitely-not-a-backend"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected cfg.Validate() to reject an unknown stt.provider selector")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "stt.provider") {
		t.Fatalf("expected a CONFIG_INVALID stt.provider error, got: %v", err)
	}
}

// TestSTTSelector_KnownAndOffPassValidate guards against over-rejection: every
// recognized selector (plus auto/off/empty) must pass config validation.
func TestSTTSelector_KnownAndOffPassValidate(t *testing.T) {
	for _, sel := range []string{"", "auto", "off", "none", "disabled", "mistral", "elevenlabs", "whisper", "openai", "gemini", "GEMINI"} {
		cfg := config.Default()
		cfg.STTProvider = sel
		if err := cfg.Validate(); err != nil {
			t.Errorf("stt.provider=%q should pass validation, got: %v", sel, err)
		}
	}
}
