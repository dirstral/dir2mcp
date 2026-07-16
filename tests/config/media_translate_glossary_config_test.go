package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaTranslateGlossary_NestedYAMLApplies locks the per-target-language,
// map-of-maps `media.translate.glossary` parse (SPEC §8.6.2, issue #574): the
// nested block is decoded with yaml.v3 (not the flat scalar/list parser) and the
// language keys are lower-cased to match the normalized target_langs used at
// lookup time.
func TestMediaTranslateGlossary_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  translate:",
		"    engine: chat",
		"    glossary:",
		"      es:",
		`        "United Nations": "Naciones Unidas"`,
		`        "Security Council": "Consejo de Seguridad"`,
		"      FR:",
		`        "United Nations": "Nations Unies"`,
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.translate.glossary) = %v, want nil", err)
	}
	es := cfg.MediaTranslateGlossary["es"]
	if es["United Nations"] != "Naciones Unidas" || es["Security Council"] != "Consejo de Seguridad" {
		t.Errorf("es glossary not parsed: %#v", es)
	}
	// Language tag is folded to lower-case ("FR" -> "fr").
	if fr := cfg.MediaTranslateGlossary["fr"]; fr["United Nations"] != "Nations Unies" {
		t.Errorf("fr glossary (lower-cased key) not parsed: %#v", cfg.MediaTranslateGlossary)
	}
	// Only the two configured languages are present.
	if len(cfg.MediaTranslateGlossary) != 2 {
		t.Errorf("expected 2 language entries, got %d: %#v", len(cfg.MediaTranslateGlossary), cfg.MediaTranslateGlossary)
	}
}

// TestMediaTranslateGlossary_DoesNotCorruptSiblingKeys guards the flat-parser
// footgun: a glossary SOURCE TERM that happens to match a real media.translate
// key (e.g. "engine") must NOT leak up and overwrite that sibling scalar. The
// glossary block is namespaced away from the flat parser (isMapSectionKey) and
// decoded separately, so the real engine value is preserved.
func TestMediaTranslateGlossary_DoesNotCorruptSiblingKeys(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  translate:",
		"    engine: chat",
		"    glossary:",
		"      de:",
		`        "engine": "Triebwerk"`,
		`        "window_lines": "Zeilenfenster"`,
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile = %v, want nil", err)
	}
	if cfg.MediaTranslateEngine != "chat" {
		t.Errorf("glossary source term corrupted media.translate.engine: got %q, want \"chat\"", cfg.MediaTranslateEngine)
	}
	if de := cfg.MediaTranslateGlossary["de"]; de["engine"] != "Triebwerk" {
		t.Errorf("glossary entry keyed on a config-like term not preserved: %#v", de)
	}
}

// TestMediaTranslateGlossary_AbsentIsNil confirms an absent/empty glossary leaves
// the field nil (no guidance = today's behaviour), including when the block is
// present but all entries are blank.
func TestMediaTranslateGlossary_AbsentIsNil(t *testing.T) {
	tmp := t.TempDir()
	for name, body := range map[string]string{
		"absent": strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			"media:",
			"  translate:",
			"    engine: chat",
		}, "\n") + "\n",
		"blank_entries": strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			"media:",
			"  translate:",
			"    glossary:",
			"      es:",
			`        "  ": "   "`,
		}, "\n") + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(tmp, name+".yaml")
			writeFile(t, path, body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile = %v, want nil", err)
			}
			if cfg.MediaTranslateGlossary != nil {
				t.Errorf("expected nil glossary, got %#v", cfg.MediaTranslateGlossary)
			}
		})
	}
}

// TestMediaTranslateGlossary_NotConfusedWithSubtitlesGlossary asserts the nested
// translate glossary and the unrelated, list-valued media.subtitles.glossary
// (§8.6.3) coexist without cross-contamination.
func TestMediaTranslateGlossary_NotConfusedWithSubtitlesGlossary(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  subtitles:",
		"    glossary:",
		"      - \"Github => GitHub\"",
		"  translate:",
		"    glossary:",
		"      es:",
		`        "color": "colour"`,
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile = %v, want nil", err)
	}
	if len(cfg.MediaSubtitlesGlossary) != 1 || cfg.MediaSubtitlesGlossary[0] != "Github => GitHub" {
		t.Errorf("subtitles.glossary (list) not parsed independently: %#v", cfg.MediaSubtitlesGlossary)
	}
	if cfg.MediaTranslateGlossary["es"]["color"] != "colour" {
		t.Errorf("translate.glossary (map) not parsed independently: %#v", cfg.MediaTranslateGlossary)
	}
}
