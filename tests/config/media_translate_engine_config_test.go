package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaTranslateEngine_DefaultIsChat confirms the translation engine defaults
// to "chat" (the pre-existing line-by-line chat-generator behavior), so enabling
// translation without naming an engine is unchanged.
func TestMediaTranslateEngine_DefaultIsChat(t *testing.T) {
	if got := config.Default().MediaTranslateEngine; got != "chat" {
		t.Fatalf("media.translate.engine must default to chat, got %q", got)
	}
}

// TestMediaTranslateEngine_WhisperWithWhisperSTTAndEnglish validates the happy
// path: engine=whisper with a whisper STT provider and English-only targets.
func TestMediaTranslateEngine_WhisperWithWhisperSTTAndEnglish(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "whisper" // credential-less self-hosted STT
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	cfg.MediaTranslateEngine = "whisper"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("engine=whisper with whisper STT + [en] must validate, got: %v", err)
	}
	if cfg.MediaTranslateEngine != "whisper" {
		t.Fatalf("engine should remain whisper after validation, got %q", cfg.MediaTranslateEngine)
	}
}

// TestMediaTranslateEngine_WhisperRequiresWhisperSTT: engine=whisper with a
// non-whisper (here: off) STT backend is CONFIG_INVALID — Whisper's translate
// task can only run on a whisper provider.
func TestMediaTranslateEngine_WhisperRequiresWhisperSTT(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "off"
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	cfg.MediaTranslateEngine = "whisper"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for engine=whisper with no whisper STT backend")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("error must be CONFIG_INVALID, got: %v", err)
	}
}

// TestMediaTranslateEngine_WhisperRejectsNonEnglishTargets: Whisper only
// translates TO English, so a non-English target with engine=whisper is
// CONFIG_INVALID.
func TestMediaTranslateEngine_WhisperRejectsNonEnglishTargets(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "whisper"
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"de"}
	cfg.MediaTranslateEngine = "whisper"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for engine=whisper with a non-English target")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("error must be CONFIG_INVALID, got: %v", err)
	}
}

// TestMediaTranslateEngine_InvalidValueRejected: an unknown engine value is an
// error (enum is chat|whisper).
func TestMediaTranslateEngine_InvalidValueRejected(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "whisper"
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	cfg.MediaTranslateEngine = "google"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for an unknown media.translate.engine value")
	}
}

// TestMediaTranslateEngine_ParsesFlatAndNestedYAML confirms both the flat key
// (media_translate_engine) and the nested block (media: translate: engine:) set
// the engine through the YAML loader, with a fully valid whisper combo.
func TestMediaTranslateEngine_ParsesFlatAndNestedYAML(t *testing.T) {
	base := []string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"stt_provider: whisper",
		"media_translate_enabled: true",
		"media_translate_target_langs: [en]",
	}
	flat := append(append([]string(nil), base...), "media_translate_engine: whisper")
	nested := append(append([]string(nil), base...),
		"media:", "  translate:", "    engine: whisper")

	for name, lines := range map[string][]string{"flat": flat, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(lines, "\n")+"\n")
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile(%s media.translate.engine): %v", name, err)
			}
			if cfg.MediaTranslateEngine != "whisper" {
				t.Fatalf("%s form did not set engine, got %q", name, cfg.MediaTranslateEngine)
			}
		})
	}
}

// TestMediaTranslateEngine_RoundTripsThroughSaveLoad confirms the engine value
// survives a save/load cycle.
func TestMediaTranslateEngine_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.STTProvider = "whisper"
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	cfg.MediaTranslateEngine = "whisper"
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaTranslateEngine != "whisper" {
		t.Fatalf("engine did not round-trip, got %q", loaded.MediaTranslateEngine)
	}
}
