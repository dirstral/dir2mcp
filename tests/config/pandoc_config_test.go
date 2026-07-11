package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestPandocExtractor_Accepted confirms ingest.extractor=pandoc is a valid value
// (#393) and survives validation unchanged.
func TestPandocExtractor_Accepted(t *testing.T) {
	cfg := config.Default()
	cfg.IngestExtractor = "pandoc"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ingest.extractor=pandoc should validate: %v", err)
	}
	if cfg.IngestExtractor != "pandoc" {
		t.Fatalf("ingest.extractor normalized to %q, want pandoc", cfg.IngestExtractor)
	}
}

// TestPandocCommand_NestedKeyLoads confirms the spec-style nested key
// ingest.pandoc.command loads into IngestPandocCommand (mirroring
// ingest.docling.command).
func TestPandocCommand_NestedKeyLoads(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  pandoc:\n    command: /opt/pandoc {input}\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.IngestPandocCommand != "/opt/pandoc {input}" {
		t.Fatalf("loaded ingest.pandoc.command = %q, want /opt/pandoc {input}", cfg.IngestPandocCommand)
	}
}

// TestPandocCommand_FlatRoundtrip verifies the flat pandoc_command key loads and
// survives a SaveFile→LoadFile roundtrip.
func TestPandocCommand_FlatRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\npandoc_command: my-pandoc {input}\ningest_extractor: pandoc\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.IngestPandocCommand != "my-pandoc {input}" {
		t.Fatalf("loaded pandoc_command = %q, want my-pandoc {input}", cfg.IngestPandocCommand)
	}
	if cfg.IngestExtractor != "pandoc" {
		t.Fatalf("loaded ingest_extractor = %q, want pandoc", cfg.IngestExtractor)
	}

	out := filepath.Join(tmp, "out.yaml")
	if err := config.SaveFile(out, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	if text := readFileString(t, out); !strings.Contains(text, "pandoc_command: my-pandoc {input}") {
		t.Fatalf("saved config missing pandoc_command:\n%s", text)
	}
	reloaded, err := config.LoadFile(out)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.IngestPandocCommand != "my-pandoc {input}" {
		t.Fatalf("roundtrip lost pandoc_command: %q", reloaded.IngestPandocCommand)
	}
}
