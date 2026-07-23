package tests

import (
	"os"
	"path/filepath"
	"strings"
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

// TestLateChunking_EntersEmbedIdentity pins issue #446 F2: the late-chunking
// mode is folded into the corpus-lifetime embed identity (SPEC 8.1.4), so
// building a corpus with ingest.late_chunking on then off (or a distributed
// worker with a different setting) re-derives rather than silently mixing
// context-pooled and chunk-then-embed vectors in one vector space.
func TestLateChunking_EntersEmbedIdentity(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")

	off := loadCfg(t, "version: 1\n")
	if off.IngestLateChunking {
		t.Fatal("precondition: late chunking must default off")
	}
	offID := off.Providers().EmbedIdentity()
	if !strings.HasSuffix(offID, "|off") {
		t.Fatalf("identity %q must end with the off late-chunking token", offID)
	}

	on := loadCfg(t, "ingest:\n  late_chunking: true\n")
	if !on.IngestLateChunking {
		t.Fatal("precondition: late chunking must be enabled")
	}
	onID := on.Providers().EmbedIdentity()
	// late_chunking is the 8th field, contextual (off by default) the 9th.
	if !strings.HasSuffix(onID, "|on|off") {
		t.Fatalf("identity %q must end with the on late-chunking token", onID)
	}
	if onID == offID {
		t.Fatalf("toggling late chunking must change the embed identity: %q == %q", onID, offID)
	}
	// The mode is the ONLY difference: strip the late-chunking token (8th field,
	// followed by the contextual token) and the rest of the identity must be
	// byte-identical, so nothing else drifted.
	if strings.TrimSuffix(onID, "|on|off") != strings.TrimSuffix(offID, "|off|off") {
		t.Fatalf("only the late-chunking token may differ: on=%q off=%q", onID, offID)
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
