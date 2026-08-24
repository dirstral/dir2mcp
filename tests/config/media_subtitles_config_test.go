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

// TestMediaSubtitlesSegmentation_DefaultsChunk pins that cue segmentation
// defaults to "chunk" (one cue per stored transcript chunk), the historical
// behavior, so nothing changes unless broadcast is explicitly selected.
func TestMediaSubtitlesSegmentation_DefaultsChunk(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaSubtitlesSegmentation != "chunk" {
		t.Fatalf("media.subtitles.segmentation default = %q, want chunk", cfg.MediaSubtitlesSegmentation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default: %v", err)
	}
	if cfg.MediaSubtitlesSegmentation != "chunk" {
		t.Fatalf("segmentation after validate = %q, want chunk", cfg.MediaSubtitlesSegmentation)
	}
}

// TestMediaSubtitlesSegmentation_NormalizesAndValidates pins case/whitespace
// normalization to lowercase, acceptance of the two valid modes, that an empty
// value normalizes to the chunk default, and that an unknown value is rejected.
func TestMediaSubtitlesSegmentation_NormalizesAndValidates(t *testing.T) {
	for _, in := range []string{"broadcast", "BROADCAST", "  Broadcast  ", "chunk"} {
		cfg := config.Default()
		cfg.MediaSubtitlesSegmentation = in
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", in, err)
		}
		want := strings.ToLower(strings.TrimSpace(in))
		if cfg.MediaSubtitlesSegmentation != want {
			t.Fatalf("segmentation %q normalized to %q, want %q", in, cfg.MediaSubtitlesSegmentation, want)
		}
	}

	empty := config.Default()
	empty.MediaSubtitlesSegmentation = ""
	if err := empty.Validate(); err != nil {
		t.Fatalf("Validate(empty segmentation): %v", err)
	}
	if empty.MediaSubtitlesSegmentation != "chunk" {
		t.Fatalf("empty segmentation should default to chunk, got %q", empty.MediaSubtitlesSegmentation)
	}

	bad := config.Default()
	bad.MediaSubtitlesSegmentation = "sentence"
	if err := bad.Validate(); err == nil {
		t.Fatalf("unknown segmentation mode should be rejected")
	}
}

// TestMediaSubtitlesSegmentation_NestedYAMLApplies locks the nested
// media.subtitles.segmentation mapping key so it binds via the YAML scanner.
func TestMediaSubtitlesSegmentation_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  subtitles:",
		"    segmentation: broadcast",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested segmentation): %v", err)
	}
	if cfg.MediaSubtitlesSegmentation != "broadcast" {
		t.Fatalf("nested segmentation = %q, want broadcast", cfg.MediaSubtitlesSegmentation)
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
	cfg.MediaSubtitlesSegmentation = "broadcast"
	cfg.MediaSubtitlesGlossary = []string{"Aju?bei=>Adzhubei"}
	cfg.MediaSubtitlesDropPhrases = []string{"Донбасс|Крым|НАТО"}
	cfg.MediaSubtitlesScrubPhrases = []string{`Крым,?\s*НАТО`}
	cfg.MediaSubtitlesExpectScript = "cyrillic"
	cfg.MediaSubtitlesCollapseRepeats = 3
	cfg.MediaSubtitlesDropURLs = true

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
	if loaded.MediaSubtitlesSegmentation != "broadcast" {
		t.Fatalf("segmentation did not round-trip: %q", loaded.MediaSubtitlesSegmentation)
	}
	if len(loaded.MediaSubtitlesGlossary) != 1 || loaded.MediaSubtitlesGlossary[0] != "Aju?bei=>Adzhubei" {
		t.Fatalf("glossary did not round-trip: %#v", loaded.MediaSubtitlesGlossary)
	}
	if len(loaded.MediaSubtitlesDropPhrases) != 1 || loaded.MediaSubtitlesDropPhrases[0] != "Донбасс|Крым|НАТО" {
		t.Fatalf("drop_phrases did not round-trip: %#v", loaded.MediaSubtitlesDropPhrases)
	}
	if len(loaded.MediaSubtitlesScrubPhrases) != 1 || loaded.MediaSubtitlesScrubPhrases[0] != `Крым,?\s*НАТО` {
		t.Fatalf("scrub_phrases did not round-trip: %#v", loaded.MediaSubtitlesScrubPhrases)
	}
	if loaded.MediaSubtitlesExpectScript != "cyrillic" {
		t.Fatalf("expect_script did not round-trip: %q", loaded.MediaSubtitlesExpectScript)
	}
	if loaded.MediaSubtitlesCollapseRepeats != 3 {
		t.Fatalf("collapse_repeats = %d, want 3", loaded.MediaSubtitlesCollapseRepeats)
	}
	if !loaded.MediaSubtitlesDropURLs {
		t.Fatalf("drop_urls did not round-trip")
	}
}

// TestMediaSubtitlesCleaning_DefaultsOff pins that the cue-cleaning passes are
// off by default (no glossary, no collapse, no URL drop).
func TestMediaSubtitlesCleaning_DefaultsOff(t *testing.T) {
	cfg := config.Default()
	if len(cfg.MediaSubtitlesGlossary) != 0 {
		t.Fatalf("glossary should default empty, got %#v", cfg.MediaSubtitlesGlossary)
	}
	if len(cfg.MediaSubtitlesDropPhrases) != 0 {
		t.Fatalf("drop_phrases should default empty, got %#v", cfg.MediaSubtitlesDropPhrases)
	}
	if len(cfg.MediaSubtitlesScrubPhrases) != 0 {
		t.Fatalf("scrub_phrases should default empty, got %#v", cfg.MediaSubtitlesScrubPhrases)
	}
	if cfg.MediaSubtitlesExpectScript != "" {
		t.Fatalf("expect_script should default empty, got %q", cfg.MediaSubtitlesExpectScript)
	}
	if cfg.MediaSubtitlesCollapseRepeats != 0 {
		t.Fatalf("collapse_repeats should default 0, got %d", cfg.MediaSubtitlesCollapseRepeats)
	}
	if cfg.MediaSubtitlesDropURLs {
		t.Fatalf("drop_urls should default false")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default: %v", err)
	}
}

// TestMediaSubtitlesCleaning_NestedYAMLApplies locks the nested
// media.subtitles.{glossary,collapse_repeats,drop_urls} mapping keys.
func TestMediaSubtitlesCleaning_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  subtitles:",
		"    glossary:",
		"      - Aju?bei=>Adzhubei",
		"      - Khruschev=>Khrushchev",
		"    drop_phrases:",
		"      - Донбасс|Крым|Украина|Иван|Плющ|НАТО",
		"    scrub_phrases:",
		"      - Донбасс.*НАТО",
		"    expect_script: cyrillic",
		"    collapse_repeats: 3",
		"    drop_urls: true",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested cleaning): %v", err)
	}
	if len(cfg.MediaSubtitlesGlossary) != 2 {
		t.Fatalf("nested glossary not applied: %#v", cfg.MediaSubtitlesGlossary)
	}
	if len(cfg.MediaSubtitlesDropPhrases) != 1 {
		t.Fatalf("nested drop_phrases not applied: %#v", cfg.MediaSubtitlesDropPhrases)
	}
	if len(cfg.MediaSubtitlesScrubPhrases) != 1 {
		t.Fatalf("nested scrub_phrases not applied: %#v", cfg.MediaSubtitlesScrubPhrases)
	}
	if cfg.MediaSubtitlesExpectScript != "cyrillic" {
		t.Fatalf("nested expect_script not applied: %q", cfg.MediaSubtitlesExpectScript)
	}
	if cfg.MediaSubtitlesCollapseRepeats != 3 {
		t.Fatalf("nested collapse_repeats = %d, want 3", cfg.MediaSubtitlesCollapseRepeats)
	}
	if !cfg.MediaSubtitlesDropURLs {
		t.Fatalf("nested drop_urls not applied")
	}
}

// TestMediaSubtitlesCleaning_RejectsBadConfig pins fail-fast validation: a
// malformed glossary entry and a negative collapse threshold are rejected.
func TestMediaSubtitlesCleaning_RejectsBadConfig(t *testing.T) {
	badGloss := config.Default()
	badGloss.MediaSubtitlesGlossary = []string{"missing-arrow"}
	if err := badGloss.Validate(); err == nil {
		t.Fatalf("malformed glossary entry should be rejected")
	}

	badRe := config.Default()
	badRe.MediaSubtitlesGlossary = []string{"a(b=>c"}
	if err := badRe.Validate(); err == nil {
		t.Fatalf("invalid glossary regexp should be rejected")
	}

	badN := config.Default()
	badN.MediaSubtitlesCollapseRepeats = -1
	if err := badN.Validate(); err == nil {
		t.Fatalf("negative collapse_repeats should be rejected")
	}

	badDrop := config.Default()
	badDrop.MediaSubtitlesDropPhrases = []string{"a(b"}
	if err := badDrop.Validate(); err == nil {
		t.Fatalf("invalid drop_phrases regexp should be rejected")
	}

	badScrub := config.Default()
	badScrub.MediaSubtitlesScrubPhrases = []string{"a(b"}
	if err := badScrub.Validate(); err == nil {
		t.Fatalf("invalid scrub_phrases regexp should be rejected")
	}

	badScript := config.Default()
	badScript.MediaSubtitlesExpectScript = "klingon"
	if err := badScript.Validate(); err == nil {
		t.Fatalf("unknown expect_script name should be rejected")
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
