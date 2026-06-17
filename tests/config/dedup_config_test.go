package tests

import (
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestDedup_DefaultOff pins SPEC 9.2: dedup.retrieval defaults to false.
func TestDedup_DefaultOff(t *testing.T) {
	cfg := config.Default()
	if cfg.DedupRetrieval {
		t.Fatalf("dedup.retrieval must default to false, got true")
	}
}

// TestDedup_NestedYAMLRoundTrips pins the nested-mapping form
// (dedup: \n  retrieval: true) parses onto Config.DedupRetrieval.
func TestDedup_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, strings.Join([]string{
		"dedup:",
		"  retrieval: true",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.DedupRetrieval {
		t.Fatalf("dedup.retrieval=true must enable DedupRetrieval, got false")
	}
}

// TestDedup_FlatAliasRoundTrips pins the flat snake_case alias
// (dedup_retrieval -> dedup.retrieval).
func TestDedup_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, "dedup_retrieval: true\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.DedupRetrieval {
		t.Fatalf("dedup_retrieval: true must enable DedupRetrieval, got false")
	}
}

// TestDedup_SnapshotRoundTrips pins the persisted-snapshot round-trip: the
// flag survives SaveEffectiveSnapshot -> on-disk YAML -> LoadEffectiveSnapshot.
func TestDedup_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.DedupRetrieval = true

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "dedup_retrieval: true") {
		t.Fatalf("snapshot must persist dedup_retrieval: true:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.DedupRetrieval {
		t.Fatalf("loaded snapshot must carry DedupRetrieval=true, got false")
	}
}
