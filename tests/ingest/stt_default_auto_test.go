package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// A fully local configuration binds embedding and chat to a local endpoint and
// supplies no cloud key. Transcription is optional (SPEC §8.2, §8.1.3): with no
// eligible STT profile it stays off, and the service must start. It used to
// fail with "stt.provider mistral-ocr: required capability but the profile has
// no credential", because the default named Mistral explicitly.
func TestDefaultSTTIsAutoAndStartsWithoutAKey(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("ELEVENLABS_API_KEY", "")
	cfg := config.Default()
	if cfg.STTProvider != "auto" {
		t.Fatalf("default stt provider = %q, want auto", cfg.STTProvider)
	}
	cfg.RootDir = t.TempDir()
	cfg.StateDir = t.TempDir()
	if _, err := ingest.NewService(cfg, &fakeIngestStore{}); err != nil {
		t.Fatalf("NewService with the default config and no keys: %v", err)
	}
	if prov, _, ok := ingest.ResolveSTTProviderModel(cfg); ok || prov != "" {
		t.Errorf("resolved STT provider = %q with no credential, want none (STT off)", prov)
	}
}

// TestDefaultSTTPicksMistralWhenItsKeyIsPresent pins backward compatibility:
// with a Mistral key, auto selects the same profile the old explicit default
// did, so an existing corpus keeps its transcript identity.
func TestDefaultSTTPicksMistralWhenItsKeyIsPresent(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	cfg := config.Default()
	explicit := cfg
	explicit.STTProvider = "mistral"
	a, am, aok := ingest.ResolveSTTProviderModel(cfg)
	b, bm, bok := ingest.ResolveSTTProviderModel(explicit)
	if !aok || !bok || a != b || am != bm {
		t.Errorf("auto resolved %q/%q, explicit mistral %q/%q: they must match", a, am, b, bm)
	}
}
