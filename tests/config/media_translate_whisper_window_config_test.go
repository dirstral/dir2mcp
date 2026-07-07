package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaTranslateWhisperWindow_DefaultIsZero confirms windowing is off by
// default, so existing corpora keep the single-pass whisper-translate behavior.
func TestMediaTranslateWhisperWindow_DefaultIsZero(t *testing.T) {
	if got := config.Default().MediaTranslateWhisperWindowSec; got != 0 {
		t.Fatalf("media.translate.whisper_window_sec must default to 0, got %d", got)
	}
}

// TestMediaTranslateWhisperWindow_ParsesFlatAndNestedYAML checks both the flat
// (media_translate_whisper_window_sec) and nested (media: translate:
// whisper_window_sec:) forms set the value through the YAML loader.
func TestMediaTranslateWhisperWindow_ParsesFlatAndNestedYAML(t *testing.T) {
	base := []string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"stt_provider: whisper",
		"media_translate_enabled: true",
		"media_translate_target_langs: [en]",
		"media_translate_engine: whisper",
	}
	flat := append(append([]string(nil), base...), "media_translate_whisper_window_sec: 45")
	nested := append(append([]string(nil), base...),
		"media:", "  translate:", "    whisper_window_sec: 45")

	for name, lines := range map[string][]string{"flat": flat, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(lines, "\n")+"\n")
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile(%s): %v", name, err)
			}
			if cfg.MediaTranslateWhisperWindowSec != 45 {
				t.Fatalf("%s form did not set window, got %d", name, cfg.MediaTranslateWhisperWindowSec)
			}
		})
	}
}

// TestMediaTranslateWhisperWindow_RejectsNegative confirms a negative window is
// CONFIG_INVALID at parse time (deterministic rejection, not silent clamping).
func TestMediaTranslateWhisperWindow_RejectsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "media_translate_whisper_window_sec: -5\n")
	if _, err := config.LoadFile(path); err == nil {
		t.Fatal("negative media.translate.whisper_window_sec must be rejected")
	}
}

// TestMediaTranslateWhisperWindow_RoundTripsThroughSaveLoad confirms the value
// survives a save/load cycle.
func TestMediaTranslateWhisperWindow_RoundTripsThroughSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaTranslateWhisperWindowSec = 45
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaTranslateWhisperWindowSec != 45 {
		t.Fatalf("window did not round-trip, got %d", loaded.MediaTranslateWhisperWindowSec)
	}
}
