package tests

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestCrossLingual_DefaultsDisabled pins the local-first, no-surprises default:
// cross-lingual query expansion is off and has no target languages unless
// explicitly configured.
func TestCrossLingual_DefaultsDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.CrossLingualEnabled {
		t.Fatalf("retrieval.cross_lingual.enabled must default to false")
	}
	if len(cfg.CrossLingualTargetLangs) != 0 {
		t.Fatalf("retrieval.cross_lingual.target_langs must default to empty, got %#v", cfg.CrossLingualTargetLangs)
	}
}

// TestCrossLingual_NestedYAMLRoundTrips pins the nested spec-style block onto
// CrossLingualEnabled + CrossLingualTargetLangs.
func TestCrossLingual_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  cross_lingual:",
		"    enabled: true",
		"    target_langs:",
		"      - ru",
		"      - en",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.CrossLingualEnabled {
		t.Fatalf("nested retrieval.cross_lingual.enabled not applied")
	}
	if !reflect.DeepEqual(cfg.CrossLingualTargetLangs, []string{"ru", "en"}) {
		t.Fatalf("nested target_langs = %#v, want [ru en]", cfg.CrossLingualTargetLangs)
	}
}

// TestCrossLingual_FlatAliasRoundTrips pins the flat snake_case aliases.
func TestCrossLingual_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"cross_lingual_enabled: true",
		"cross_lingual_target_langs:",
		"  - auto",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.CrossLingualEnabled {
		t.Fatalf("flat cross_lingual_enabled not applied")
	}
	if !reflect.DeepEqual(cfg.CrossLingualTargetLangs, []string{"auto"}) {
		t.Fatalf("flat cross_lingual_target_langs = %#v, want [auto]", cfg.CrossLingualTargetLangs)
	}
}

// TestCrossLingual_AcceptsAutoSentinelAndExplicit pins that the "auto" sentinel
// is accepted alongside BCP-47 tags and survives normalization (lower-cased,
// de-duplicated).
func TestCrossLingual_AcceptsAutoSentinelAndExplicit(t *testing.T) {
	cfg := config.Default()
	cfg.CrossLingualTargetLangs = []string{"AUTO", "RU", "ru", "pt-BR"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with valid target_langs = %v, want nil", err)
	}
	if !reflect.DeepEqual(cfg.CrossLingualTargetLangs, []string{"auto", "ru", "pt-br"}) {
		t.Fatalf("target_langs not normalized/deduped: %#v", cfg.CrossLingualTargetLangs)
	}
}

// TestCrossLingual_RejectsInvalidTag pins that a tag outside the cache-safe
// BCP-47 alphabet (and not the "auto" sentinel) is CONFIG_INVALID.
func TestCrossLingual_RejectsInvalidTag(t *testing.T) {
	cfg := config.Default()
	cfg.CrossLingualTargetLangs = []string{"en_us"} // underscore is not allowed
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Validate with invalid target_langs = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "retrieval.cross_lingual.target_langs") {
		t.Fatalf("error must mention retrieval.cross_lingual.target_langs, got: %v", err)
	}
}

// TestCrossLingual_SnapshotRoundTrips pins the persisted-snapshot round-trip for
// both the enable flag and the target list.
func TestCrossLingual_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.CrossLingualEnabled = true
	cfg.CrossLingualTargetLangs = []string{"ru", "en"}

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "cross_lingual_enabled: true") {
		t.Fatalf("snapshot must persist cross_lingual_enabled: true:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.CrossLingualEnabled {
		t.Fatalf("loaded snapshot must carry CrossLingualEnabled=true")
	}
	if !reflect.DeepEqual(loaded.CrossLingualTargetLangs, []string{"ru", "en"}) {
		t.Fatalf("loaded snapshot target_langs = %#v, want [ru en]", loaded.CrossLingualTargetLangs)
	}
}
