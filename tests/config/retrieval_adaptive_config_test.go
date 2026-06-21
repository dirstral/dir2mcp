package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRetrievalAdaptive_DefaultDisabled pins the opt-in default: the adaptive
// gate is off and its k bounds carry the built-in window unless configured.
func TestRetrievalAdaptive_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalAdaptiveEnabled {
		t.Fatalf("retrieval.adaptive.enabled must default to false")
	}
	if cfg.RetrievalAdaptiveKMin != 4 {
		t.Fatalf("retrieval.adaptive.k_min default = %d, want 4", cfg.RetrievalAdaptiveKMin)
	}
	if cfg.RetrievalAdaptiveKMax != 30 {
		t.Fatalf("retrieval.adaptive.k_max default = %d, want 30", cfg.RetrievalAdaptiveKMax)
	}
}

// TestRetrievalAdaptive_NestedYAMLRoundTrips pins the nested-mapping form onto
// the Config fields.
func TestRetrievalAdaptive_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  adaptive:",
		"    enabled: true",
		"    k_min: 3",
		"    k_max: 25",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalAdaptiveEnabled {
		t.Fatalf("nested retrieval.adaptive.enabled did not round-trip")
	}
	if cfg.RetrievalAdaptiveKMin != 3 || cfg.RetrievalAdaptiveKMax != 25 {
		t.Fatalf("nested k bounds = (%d,%d), want (3,25)", cfg.RetrievalAdaptiveKMin, cfg.RetrievalAdaptiveKMax)
	}
}

// TestRetrievalAdaptive_FlatAliasRoundTrips pins the flat snake_case aliases.
func TestRetrievalAdaptive_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_adaptive_enabled: true",
		"retrieval_adaptive_k_min: 5",
		"retrieval_adaptive_k_max: 20",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalAdaptiveEnabled || cfg.RetrievalAdaptiveKMin != 5 || cfg.RetrievalAdaptiveKMax != 20 {
		t.Fatalf("flat aliases = (%t,%d,%d), want (true,5,20)", cfg.RetrievalAdaptiveEnabled, cfg.RetrievalAdaptiveKMin, cfg.RetrievalAdaptiveKMax)
	}
}

// TestRetrievalAdaptive_SnapshotRoundTrips pins the persisted-snapshot
// round-trip: the gate settings survive SaveEffectiveSnapshot -> YAML -> load.
func TestRetrievalAdaptive_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalAdaptiveEnabled = true
	cfg.RetrievalAdaptiveKMin = 6
	cfg.RetrievalAdaptiveKMax = 18

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, want := range []string{
		"retrieval_adaptive_enabled: true",
		"retrieval_adaptive_k_min: 6",
		"retrieval_adaptive_k_max: 18",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot must persist %q:\n%s", want, raw)
		}
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.RetrievalAdaptiveEnabled || loaded.RetrievalAdaptiveKMin != 6 || loaded.RetrievalAdaptiveKMax != 18 {
		t.Fatalf("loaded snapshot = (%t,%d,%d), want (true,6,18)", loaded.RetrievalAdaptiveEnabled, loaded.RetrievalAdaptiveKMin, loaded.RetrievalAdaptiveKMax)
	}
}

// TestRetrievalAdaptive_RejectsNegativeKBound pins validation: a negative bound
// is CONFIG_INVALID when the gate is enabled.
func TestRetrievalAdaptive_RejectsNegativeKBound(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_adaptive_enabled: true",
		"retrieval_adaptive_k_min: -1",
		"",
	}, "\n"))

	// A negative integer is rejected at parse time for non-negative keys.
	if _, err := config.LoadFile(path); err == nil {
		t.Fatalf("LoadFile with negative k_min = nil error, want rejection")
	}
}

// TestRetrievalAdaptive_RejectsInvertedWindow pins validation: k_min > k_max is
// rejected when the gate is enabled, because clamping would be ill-defined.
func TestRetrievalAdaptive_RejectsInvertedWindow(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalAdaptiveEnabled = true
	cfg.RetrievalAdaptiveKMin = 20
	cfg.RetrievalAdaptiveKMax = 5
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Validate with inverted window = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "retrieval.adaptive") {
		t.Fatalf("error must mention retrieval.adaptive, got: %v", err)
	}
}

// TestRetrievalAdaptive_DisabledSkipsBoundValidation pins that an inverted
// window is tolerated while the gate is disabled (the bounds are ignored), so a
// stale config never blocks startup.
func TestRetrievalAdaptive_DisabledSkipsBoundValidation(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalAdaptiveEnabled = false
	cfg.RetrievalAdaptiveKMin = 20
	cfg.RetrievalAdaptiveKMax = 5
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with disabled gate must ignore k bounds, got: %v", err)
	}
}
