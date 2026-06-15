package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestMediaVariants_DefaultsDisabledSelectBest(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaVariantsGroup {
		t.Fatalf("media.variants.group must default to false")
	}
	if cfg.MediaVariantsSelect != "best" {
		t.Fatalf("media.variants.select default = %q, want best", cfg.MediaVariantsSelect)
	}
}

func TestMediaVariants_ValidateRejectsUnknownSelect(t *testing.T) {
	cfg := config.Default()
	cfg.MediaVariantsSelect = "smallest"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unknown media.variants.select")
	}
}

func TestMediaVariants_ValidateNormalizesSelect(t *testing.T) {
	cfg := config.Default()
	cfg.MediaVariantsSelect = "FIRST"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("first must be a valid select policy: %v", err)
	}
	if cfg.MediaVariantsSelect != "first" {
		t.Fatalf("media.variants.select should normalize to lowercase, got %q", cfg.MediaVariantsSelect)
	}
}

func TestMediaVariants_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaVariantsGroup = true
	cfg.MediaVariantsSelect = "first"
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if !loaded.MediaVariantsGroup {
		t.Fatalf("media.variants.group did not round-trip")
	}
	if loaded.MediaVariantsSelect != "first" {
		t.Fatalf("media.variants.select did not round-trip: got %q", loaded.MediaVariantsSelect)
	}
}

// TestMediaLeadingSilence_RoundTripsThroughSaveLoad exercises the new
// leading-silence/VAD scalars (dir2mcp#258), including the float plumbing for a
// negative dB threshold (setFloatFileScalar/floatPtr), through save/load.
func TestMediaLeadingSilence_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaTrimLeadingSilence = true
	cfg.MediaSilenceThresholdDB = -42.5
	cfg.MediaVAD = true
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if !loaded.MediaTrimLeadingSilence {
		t.Errorf("media.trim_leading_silence did not round-trip")
	}
	if loaded.MediaSilenceThresholdDB != -42.5 {
		t.Errorf("media.silence_threshold_db did not round-trip: got %v, want -42.5", loaded.MediaSilenceThresholdDB)
	}
	if !loaded.MediaVAD {
		t.Errorf("media.vad did not round-trip")
	}
}

// TestMediaLeadingSilence_Defaults locks the opt-in defaults: trimming and VAD
// off, threshold left at zero so avutil applies its own -40 dB default.
func TestMediaLeadingSilence_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaTrimLeadingSilence {
		t.Errorf("media.trim_leading_silence must default to false")
	}
	if cfg.MediaVAD {
		t.Errorf("media.vad must default to false")
	}
	if cfg.MediaSilenceThresholdDB != 0 {
		t.Errorf("media.silence_threshold_db default = %v, want 0 (avutil default applies)", cfg.MediaSilenceThresholdDB)
	}
}

// TestMediaSilenceThreshold_NestedYAMLApplies asserts the nested media: block
// carries the negative-dB float scalar through the bespoke YAML scanner.
func TestMediaSilenceThreshold_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  trim_leading_silence: true",
		"  silence_threshold_db: -40",
		"  vad: true",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media) = %v, want nil", err)
	}
	if !cfg.MediaTrimLeadingSilence {
		t.Errorf("nested media.trim_leading_silence not applied")
	}
	if cfg.MediaSilenceThresholdDB != -40 {
		t.Errorf("nested media.silence_threshold_db = %v, want -40", cfg.MediaSilenceThresholdDB)
	}
	if !cfg.MediaVAD {
		t.Errorf("nested media.vad not applied")
	}
}

// TestMediaVariants_NestedYAMLApplies locks the nested spec-style block
// (media: -> variants: -> group/select) so it is actually applied rather than
// silently falling back to defaults (regression: isMapSectionKey must recognize
// "media" and "media.variants").
func TestMediaVariants_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  variants:",
		"    group: true",
		"    select: first",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.variants) = %v, want nil", err)
	}
	if !cfg.MediaVariantsGroup {
		t.Errorf("nested media.variants.group not applied: got false, want true")
	}
	if cfg.MediaVariantsSelect != "first" {
		t.Errorf("nested media.variants.select not applied: got %q, want first", cfg.MediaVariantsSelect)
	}
}
