package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaSTTOnUncoveredLanguage_Default confirms the honest-coverage floor
// action defaults to the fail-open "warn" (SPEC §8.2.1, #566): behavior is
// unchanged unless an operator opts into strict skipping.
func TestMediaSTTOnUncoveredLanguage_Default(t *testing.T) {
	if got := config.Default().MediaSTTOnUncoveredLanguage; got != "warn" {
		t.Fatalf("default media.stt.on_uncovered_language = %q, want warn", got)
	}
}

// TestMediaSTTOnUncoveredLanguage_Validation normalizes empty to warn, normalizes
// case, accepts skip, and rejects any value outside warn/skip.
func TestMediaSTTOnUncoveredLanguage_Validation(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSTTOnUncoveredLanguage = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty on_uncovered_language should validate: %v", err)
	}
	if cfg.MediaSTTOnUncoveredLanguage != "warn" {
		t.Fatalf("empty on_uncovered_language should normalize to warn, got %q", cfg.MediaSTTOnUncoveredLanguage)
	}

	cfg = config.Default()
	cfg.MediaSTTOnUncoveredLanguage = "SKIP"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("skip on_uncovered_language should validate: %v", err)
	}
	if cfg.MediaSTTOnUncoveredLanguage != "skip" {
		t.Fatalf("on_uncovered_language should normalize case to skip, got %q", cfg.MediaSTTOnUncoveredLanguage)
	}

	cfg = config.Default()
	cfg.MediaSTTOnUncoveredLanguage = "drop"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for an unknown media.stt.on_uncovered_language value, got nil")
	}
}

// TestMediaSTTOnUncoveredLanguage_ParsesFlatAndNestedYAML confirms both the flat
// key (media_stt_on_uncovered_language) and the nested block
// (media: stt: on_uncovered_language:) set the action through the YAML loader.
func TestMediaSTTOnUncoveredLanguage_ParsesFlatAndNestedYAML(t *testing.T) {
	base := []string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
	}
	flat := append(append([]string(nil), base...), "media_stt_on_uncovered_language: skip")
	nested := append(append([]string(nil), base...),
		"media:", "  stt:", "    on_uncovered_language: skip")

	for name, lines := range map[string][]string{"flat": flat, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(lines, "\n")+"\n")
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile(%s media.stt.on_uncovered_language): %v", name, err)
			}
			if cfg.MediaSTTOnUncoveredLanguage != "skip" {
				t.Fatalf("%s form did not set the action, got %q", name, cfg.MediaSTTOnUncoveredLanguage)
			}
		})
	}
}

// TestMediaSTTOnUncoveredLanguage_RoundTripsThroughSaveLoad confirms the action
// survives a save/load cycle.
func TestMediaSTTOnUncoveredLanguage_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaSTTOnUncoveredLanguage = "skip"
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if text := readFileString(t, path); !strings.Contains(text, "media_stt_on_uncovered_language: skip") {
		t.Fatalf("saved config missing media_stt_on_uncovered_language:\n%s", text)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaSTTOnUncoveredLanguage != "skip" {
		t.Fatalf("action did not round-trip, got %q", loaded.MediaSTTOnUncoveredLanguage)
	}
}
