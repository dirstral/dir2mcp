package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRetrievalRecencyHalfLife_DefaultDisabled pins the local-first default: the
// time-decay is 0 (disabled) unless explicitly configured.
func TestRetrievalRecencyHalfLife_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalRecencyHalfLife != 0 {
		t.Fatalf("retrieval.recency_half_life must default to 0 (disabled), got %v", cfg.RetrievalRecencyHalfLife)
	}
}

// TestRetrievalRecencyHalfLife_NestedYAMLRoundTrips pins the nested-mapping form
// (retrieval: \n  recency_half_life: 720h) onto Config.RetrievalRecencyHalfLife.
func TestRetrievalRecencyHalfLife_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  recency_half_life: 720h",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalRecencyHalfLife != 720*time.Hour {
		t.Fatalf("nested retrieval.recency_half_life = %v, want 720h", cfg.RetrievalRecencyHalfLife)
	}
}

// TestRetrievalRecencyHalfLife_FlatAliasRoundTrips pins the flat snake_case
// alias (retrieval_recency_half_life -> retrieval.recency_half_life).
func TestRetrievalRecencyHalfLife_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_recency_half_life: 48h\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalRecencyHalfLife != 48*time.Hour {
		t.Fatalf("flat retrieval_recency_half_life = %v, want 48h", cfg.RetrievalRecencyHalfLife)
	}
}

// TestRetrievalRecencyHalfLife_SnapshotRoundTrips pins the persisted-snapshot
// round-trip: the half-life survives SaveEffectiveSnapshot -> YAML -> load.
func TestRetrievalRecencyHalfLife_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalRecencyHalfLife = 168 * time.Hour

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "retrieval_recency_half_life: 168h") {
		t.Fatalf("snapshot must persist retrieval_recency_half_life: 168h:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if loaded.RetrievalRecencyHalfLife != 168*time.Hour {
		t.Fatalf("loaded snapshot must carry RetrievalRecencyHalfLife=168h, got %v", loaded.RetrievalRecencyHalfLife)
	}
}

// TestRetrievalRecencyHalfLife_RejectsUnparseable asserts a non-duration value
// is rejected at config-parse time rather than silently ignored.
func TestRetrievalRecencyHalfLife_RejectsUnparseable(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_recency_half_life: not-a-duration\n")

	if _, err := config.LoadFile(path); err == nil {
		t.Fatalf("LoadFile with unparseable retrieval_recency_half_life = nil error, want rejection")
	}
}

// TestRetrievalRecencyHalfLife_RejectsNegative pins validation: a negative
// half-life is CONFIG_INVALID (it would amplify rather than decay older content,
// so it is a misconfiguration).
func TestRetrievalRecencyHalfLife_RejectsNegative(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_recency_half_life: -1h\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatalf("LoadFile with negative retrieval_recency_half_life = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "retrieval.recency_half_life") {
		t.Fatalf("error must mention retrieval.recency_half_life, got: %v", err)
	}
}

// TestRetrievalRecencyHalfLife_ValidateRejectsNegativeProgrammatic pins that the
// Validate() guard rejects a negative half-life injected programmatically (not
// via the file parser), so the decay can never run with a bad half-life.
func TestRetrievalRecencyHalfLife_ValidateRejectsNegativeProgrammatic(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalRecencyHalfLife = -time.Hour
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate with negative RetrievalRecencyHalfLife = nil error, want rejection")
	}
}
