package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaSubtitles_DefaultsOff confirms the optional bilingual subtitle export
// surface (SPEC §8.6.10) is OFF by default and carries the spec default align
// tolerance, so behavior is unchanged when nothing is configured.
func TestMediaSubtitles_DefaultsOff(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaSubtitlesTTMLEnabled {
		t.Fatalf("media.subtitles.ttml.enabled must default to false")
	}
	if cfg.MediaSubtitlesSMILEnabled {
		t.Fatalf("media.subtitles.smil.enabled must default to false")
	}
	if cfg.MediaSubtitlesTTMLAlignToleranceMS != config.DefaultMediaSubtitlesAlignToleranceMS {
		t.Fatalf("align tolerance default = %d, want %d",
			cfg.MediaSubtitlesTTMLAlignToleranceMS, config.DefaultMediaSubtitlesAlignToleranceMS)
	}
}

// TestMediaSubtitles_RoundTrip pins SaveFile/LoadFile round-trip of the new
// keys so the snapshot faithfully persists an enabled config.
func TestMediaSubtitles_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaSubtitlesTTMLEnabled = true
	cfg.MediaSubtitlesSMILEnabled = true
	cfg.MediaSubtitlesTTMLAlignToleranceMS = 1800

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !loaded.MediaSubtitlesTTMLEnabled || !loaded.MediaSubtitlesSMILEnabled {
		t.Fatalf("enabled flags did not round-trip: %+v", loaded)
	}
	if loaded.MediaSubtitlesTTMLAlignToleranceMS != 1800 {
		t.Fatalf("align tolerance = %d, want 1800", loaded.MediaSubtitlesTTMLAlignToleranceMS)
	}
}

// TestMediaSubtitles_NestedYAMLApplies locks the nested media.subtitles.ttml /
// media.subtitles.smil mapping so child keys are bound via the YAML scanner.
func TestMediaSubtitles_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  subtitles:",
		"    ttml:",
		"      enabled: true",
		"      align_tolerance_ms: 1200",
		"    smil:",
		"      enabled: true",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.subtitles): %v", err)
	}
	if !cfg.MediaSubtitlesTTMLEnabled || !cfg.MediaSubtitlesSMILEnabled {
		t.Fatalf("nested enable flags not applied: %+v", cfg)
	}
	if cfg.MediaSubtitlesTTMLAlignToleranceMS != 1200 {
		t.Fatalf("nested align_tolerance_ms = %d, want 1200", cfg.MediaSubtitlesTTMLAlignToleranceMS)
	}
}

// TestMediaSubtitles_ZeroToleranceDefaults pins that an explicit 0 align
// tolerance normalizes to the spec default at validation time.
func TestMediaSubtitles_ZeroToleranceDefaults(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSubtitlesTTMLAlignToleranceMS = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.MediaSubtitlesTTMLAlignToleranceMS != config.DefaultMediaSubtitlesAlignToleranceMS {
		t.Fatalf("zero tolerance should default, got %d", cfg.MediaSubtitlesTTMLAlignToleranceMS)
	}
}

// TestMediaSubtitles_RejectsNegativeTolerance pins that a negative align
// tolerance is rejected at config-parse time rather than silently accepted.
func TestMediaSubtitles_RejectsNegativeTolerance(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media_subtitles_ttml_align_tolerance_ms: -1",
	}, "\n")+"\n")

	if _, err := config.LoadFile(path); err == nil {
		t.Fatalf("negative align tolerance should be rejected")
	}
}
