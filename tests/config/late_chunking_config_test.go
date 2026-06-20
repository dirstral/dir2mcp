package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestLateChunking_DefaultsOff asserts late chunking is opt-in: the baseline
// config has it disabled so ingestion chunks-then-embeds exactly as before
// (issue #332).
func TestLateChunking_DefaultsOff(t *testing.T) {
	if config.Default().IngestLateChunking {
		t.Fatal("IngestLateChunking should default to false (opt-in)")
	}
}

// TestLateChunking_SaveLoadRoundTrip verifies the flag survives a
// SaveFile/LoadFile cycle through the persisted YAML.
func TestLateChunking_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.IngestLateChunking = true

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !loaded.IngestLateChunking {
		t.Fatal("IngestLateChunking did not round-trip: got false, want true")
	}
}

// TestLateChunking_NestedAndFlatKeys verifies the canonical nested key
// (ingest.late_chunking) and the flat aliases load the flag.
func TestLateChunking_NestedAndFlatKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "nested", yaml: "ingest:\n  late_chunking: true\n"},
		{name: "flat_alias", yaml: "late_chunking: true\n"},
		{name: "flat_prefixed", yaml: "ingest_late_chunking: true\n"},
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
			if !cfg.IngestLateChunking {
				t.Fatalf("expected IngestLateChunking=true for %s yaml", tc.name)
			}
		})
	}
}

// TestLateChunking_EnvOverride verifies DIR2MCP_INGEST_LATE_CHUNKING flips the
// flag.
func TestLateChunking_EnvOverride(t *testing.T) {
	// Pin source/index so ambient DIR2MCP_* in CI/dev shells can't perturb the load.
	t.Setenv("DIR2MCP_SOURCE_KIND", "local")
	t.Setenv("DIR2MCP_INDEX_BACKEND", "memory")
	t.Setenv("DIR2MCP_INGEST_LATE_CHUNKING", "true")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IngestLateChunking {
		t.Fatal("DIR2MCP_INGEST_LATE_CHUNKING=true did not enable IngestLateChunking")
	}
}
