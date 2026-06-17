package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestMediaDiarize_DefaultIsAuto confirms diarization defaults to the tri-state
// "auto" (nil pointer), neither forced on nor off (SPEC §8.6.8).
func TestMediaDiarize_DefaultIsAuto(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaDiarizeEnabled != nil {
		t.Fatalf("media.diarize.enabled must default to nil (auto), got %v", *cfg.MediaDiarizeEnabled)
	}
}

// TestMediaDiarize_OmitValidatesWithAnyBackend: with diarization omitted (auto),
// validation passes regardless of the STT backend (it simply stays off on an
// incapable backend).
func TestMediaDiarize_OmitValidatesWithAnyBackend(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "off"
	cfg.MediaDiarizeEnabled = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("omitted diarize must validate, got: %v", err)
	}
}

// TestMediaDiarize_FalseValidatesWithAnyBackend: forced-off always validates.
func TestMediaDiarize_FalseValidatesWithAnyBackend(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "off"
	f := false
	cfg.MediaDiarizeEnabled = &f
	if err := cfg.Validate(); err != nil {
		t.Fatalf("diarize=false must validate, got: %v", err)
	}
}

// TestMediaDiarize_TrueWithCapableBackend: enabled:true with a
// diarization-capable STT backend (self-hosted whisper) validates.
func TestMediaDiarize_TrueWithCapableBackend(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "whisper" // credential-less, advertises CapDiarize
	tr := true
	cfg.MediaDiarizeEnabled = &tr
	if err := cfg.Validate(); err != nil {
		t.Fatalf("diarize=true with a capable backend must validate, got: %v", err)
	}
}

// TestMediaDiarize_TrueWithNoBackend_ConfigInvalid: enabled:true with STT off
// (no backend to diarize) is CONFIG_INVALID (SPEC §8.6.8).
func TestMediaDiarize_TrueWithNoBackend_ConfigInvalid(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "off"
	tr := true
	cfg.MediaDiarizeEnabled = &tr
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for diarize=true with no STT backend")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("error must be CONFIG_INVALID, got: %v", err)
	}
}

// TestMediaDiarize_TrueWithIncapableBackend_ConfigInvalid: enabled:true with an
// eligible but diarization-incapable STT backend (Mistral, kind=mistral) is
// CONFIG_INVALID. A credential is provided so the profile resolves (eligible)
// but the capability matrix does not advertise CapDiarize for it.
func TestMediaDiarize_TrueWithIncapableBackend_ConfigInvalid(t *testing.T) {
	// Sanity: confirm the test's premise — mistral kind is NOT diarize-capable
	// while whisper is — so this test fails loudly if the matrix changes.
	if provider.Can(provider.KindMistral, provider.CapDiarize) != provider.Unsupported {
		t.Fatal("precondition: mistral must not advertise CapDiarize")
	}
	if provider.Can(provider.KindWhisper, provider.CapDiarize) == provider.Unsupported {
		t.Fatal("precondition: whisper must advertise CapDiarize")
	}
	t.Setenv("MISTRAL_API_KEY", "test-key-so-mistral-ocr-is-eligible")
	cfg := config.Default()
	cfg.STTProvider = "mistral"
	tr := true
	cfg.MediaDiarizeEnabled = &tr
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for diarize=true with an incapable STT backend")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("error must be CONFIG_INVALID, got: %v", err)
	}
}

// TestMediaDiarize_RoundTripsThroughSaveLoad confirms an explicit tri-state
// value survives a save/load cycle (false is preserved, not collapsed to auto).
func TestMediaDiarize_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.STTProvider = "whisper"
	tr := true
	cfg.MediaDiarizeEnabled = &tr
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaDiarizeEnabled == nil || !*loaded.MediaDiarizeEnabled {
		t.Fatalf("diarize=true did not round-trip, got %v", loaded.MediaDiarizeEnabled)
	}
}
