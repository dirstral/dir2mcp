package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestScanCache_DefaultsOff asserts the scan cache is opt-in: the baseline
// config has it disabled (safe full-walk behavior).
func TestScanCache_DefaultsOff(t *testing.T) {
	if config.Default().IngestScanCache {
		t.Fatal("IngestScanCache should default to false (opt-in)")
	}
}

// TestScanCache_SaveLoadRoundTrip verifies the flag survives a SaveFile/LoadFile
// cycle through the persisted YAML.
func TestScanCache_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.IngestScanCache = true

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !loaded.IngestScanCache {
		t.Fatal("IngestScanCache did not round-trip: got false, want true")
	}
}

// TestScanCache_NestedAndFlatKeys verifies both the canonical nested key
// (ingest.scan_cache) and the flat alias (scan_cache) load the flag.
func TestScanCache_NestedAndFlatKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "nested", yaml: "ingest:\n  scan_cache: true\n"},
		{name: "flat_alias", yaml: "scan_cache: true\n"},
		{name: "flat_prefixed", yaml: "ingest_scan_cache: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if !cfg.IngestScanCache {
				t.Fatalf("expected IngestScanCache=true for %s yaml", tc.name)
			}
		})
	}
}

// TestScanCache_EnvOverride verifies DIR2MCP_INGEST_SCAN_CACHE flips the flag.
func TestScanCache_EnvOverride(t *testing.T) {
	// Pin source/index so ambient DIR2MCP_* in CI/dev shells can't perturb the load.
	t.Setenv("DIR2MCP_SOURCE_KIND", "local")
	t.Setenv("DIR2MCP_INDEX_BACKEND", "memory")
	t.Setenv("DIR2MCP_INGEST_SCAN_CACHE", "true")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IngestScanCache {
		t.Fatal("DIR2MCP_INGEST_SCAN_CACHE=true did not enable IngestScanCache")
	}
}

// TestDiscoveryWorkers_DefaultLoadAndBounds pins ingest.discovery_workers
// (SPEC §7.1, 0.94.0, #1159): default 1 (sequential), the nested key loads and
// round-trips, 0 is the default, and anything outside 0-64 is CONFIG_INVALID.
func TestDiscoveryWorkers_DefaultLoadAndBounds(t *testing.T) {
	if got := config.Default().IngestDiscoveryWorkers; got != 1 {
		t.Fatalf("default ingest.discovery_workers = %d, want 1", got)
	}
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(path, []byte("ingest:\n  discovery_workers: 16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil || cfg.IngestDiscoveryWorkers != 16 {
		t.Fatalf("nested key: workers=%d err=%v, want 16", cfg.IngestDiscoveryWorkers, err)
	}
	saved := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := config.SaveFile(saved, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if again, err := config.LoadFile(saved); err != nil || again.IngestDiscoveryWorkers != 16 {
		t.Fatalf("round trip: workers=%d err=%v, want 16", again.IngestDiscoveryWorkers, err)
	}
	for n, ok := range map[int]bool{0: true, 1: true, 64: true, -1: false, 65: false} {
		c := config.Default()
		c.IngestDiscoveryWorkers = n
		if err := c.Validate(); (err == nil) != ok {
			t.Fatalf("discovery_workers=%d: Validate err=%v, want ok=%v", n, err, ok)
		}
	}
}
