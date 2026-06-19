package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRetrievalHyDE_DefaultDisabled pins the local-first default: the HyDE query
// transform is off and the mode defaults to "fuse" unless explicitly configured.
func TestRetrievalHyDE_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalHyDEEnabled {
		t.Fatalf("retrieval.hyde.enabled must default to false, got true")
	}
	if cfg.RetrievalHyDEMode != config.HyDEModeFuse {
		t.Fatalf("retrieval.hyde.mode must default to %q, got %q", config.HyDEModeFuse, cfg.RetrievalHyDEMode)
	}
}

// TestRetrievalHyDE_NestedYAMLRoundTrips pins the nested-mapping form
// (retrieval: \n  hyde: \n    enabled: true \n    mode: replace).
func TestRetrievalHyDE_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  hyde:",
		"    enabled: true",
		"    mode: replace",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalHyDEEnabled {
		t.Fatalf("nested retrieval.hyde.enabled = false, want true")
	}
	if cfg.RetrievalHyDEMode != config.HyDEModeReplace {
		t.Fatalf("nested retrieval.hyde.mode = %q, want %q", cfg.RetrievalHyDEMode, config.HyDEModeReplace)
	}
}

// TestRetrievalHyDE_FlatAliasRoundTrips pins the flat snake_case aliases
// (retrieval_hyde_enabled / retrieval_hyde_mode).
func TestRetrievalHyDE_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_hyde_enabled: true",
		"retrieval_hyde_mode: fuse",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalHyDEEnabled {
		t.Fatalf("flat retrieval_hyde_enabled = false, want true")
	}
	if cfg.RetrievalHyDEMode != config.HyDEModeFuse {
		t.Fatalf("flat retrieval_hyde_mode = %q, want %q", cfg.RetrievalHyDEMode, config.HyDEModeFuse)
	}
}

// TestRetrievalHyDE_SnapshotRoundTrips pins the persisted-snapshot round-trip:
// the enable flag and mode survive SaveEffectiveSnapshot -> YAML -> load.
func TestRetrievalHyDE_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalHyDEEnabled = true
	cfg.RetrievalHyDEMode = config.HyDEModeReplace

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "retrieval_hyde_enabled: true") {
		t.Fatalf("snapshot must persist retrieval_hyde_enabled: true:\n%s", raw)
	}
	if !strings.Contains(string(raw), "retrieval_hyde_mode: replace") {
		t.Fatalf("snapshot must persist retrieval_hyde_mode: replace:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.RetrievalHyDEEnabled || loaded.RetrievalHyDEMode != config.HyDEModeReplace {
		t.Fatalf("loaded snapshot must carry enabled=true mode=replace, got enabled=%v mode=%q",
			loaded.RetrievalHyDEEnabled, loaded.RetrievalHyDEMode)
	}
}

// TestRetrievalHyDE_EmptyModeNormalizesToFuse pins that an empty mode normalizes
// to "fuse" during validation rather than failing or behaving undefined.
func TestRetrievalHyDE_EmptyModeNormalizesToFuse(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_hyde_enabled: true\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalHyDEMode != config.HyDEModeFuse {
		t.Fatalf("empty retrieval.hyde.mode must normalize to %q, got %q", config.HyDEModeFuse, cfg.RetrievalHyDEMode)
	}
}

// TestRetrievalHyDE_RejectsUnknownMode pins validation: an unrecognized mode is
// CONFIG_INVALID so a typo fails loudly at config time.
func TestRetrievalHyDE_RejectsUnknownMode(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_hyde_mode: bogus\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatalf("LoadFile with retrieval_hyde_mode=bogus = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "retrieval.hyde.mode") {
		t.Fatalf("error must mention retrieval.hyde.mode, got: %v", err)
	}
}

// TestRetrievalHyDE_ValidateRejectsUnknownModeProgrammatic pins that Validate()
// rejects an unknown mode injected programmatically (not via the file parser).
func TestRetrievalHyDE_ValidateRejectsUnknownModeProgrammatic(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalHyDEMode = "nonsense"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate with bogus RetrievalHyDEMode = nil error, want rejection")
	}
}

// TestRetrievalHyDE_ModeCaseInsensitive pins that mode values are normalized
// case-insensitively (e.g. "REPLACE" -> "replace").
func TestRetrievalHyDE_ModeCaseInsensitive(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_hyde_mode: REPLACE\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalHyDEMode != config.HyDEModeReplace {
		t.Fatalf("REPLACE must normalize to %q, got %q", config.HyDEModeReplace, cfg.RetrievalHyDEMode)
	}
}
