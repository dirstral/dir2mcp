package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestOnUnsupported_Default confirms ingest.on_unsupported defaults to the
// lenient, backward-compatible degradation mode (§7.4.B.2).
func TestOnUnsupported_Default(t *testing.T) {
	if got := config.Default().IngestOnUnsupported; got != "lenient" {
		t.Fatalf("default ingest.on_unsupported = %q, want lenient", got)
	}
}

// TestOnUnsupported_Validation normalizes empty to lenient and rejects any value
// outside lenient/strict.
func TestOnUnsupported_Validation(t *testing.T) {
	cfg := config.Default()
	cfg.IngestOnUnsupported = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty on_unsupported should validate: %v", err)
	}
	if cfg.IngestOnUnsupported != "lenient" {
		t.Fatalf("empty on_unsupported should normalize to lenient, got %q", cfg.IngestOnUnsupported)
	}

	cfg = config.Default()
	cfg.IngestOnUnsupported = "STRICT"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("strict on_unsupported should validate: %v", err)
	}
	if cfg.IngestOnUnsupported != "strict" {
		t.Fatalf("on_unsupported should normalize case to strict, got %q", cfg.IngestOnUnsupported)
	}

	cfg = config.Default()
	cfg.IngestOnUnsupported = "loud"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for unknown on_unsupported value, got nil")
	}
}

// TestOnUnsupported_FileRoundtrip verifies the key loads from a config file and
// survives a SaveFile→LoadFile roundtrip.
func TestOnUnsupported_FileRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest_on_unsupported: strict\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.IngestOnUnsupported != "strict" {
		t.Fatalf("loaded ingest.on_unsupported = %q, want strict", cfg.IngestOnUnsupported)
	}

	out := filepath.Join(tmp, "out.yaml")
	if err := config.SaveFile(out, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	if text := readFileString(t, out); !strings.Contains(text, "ingest_on_unsupported: strict") {
		t.Fatalf("saved config missing ingest_on_unsupported:\n%s", text)
	}
	reloaded, err := config.LoadFile(out)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.IngestOnUnsupported != "strict" {
		t.Fatalf("roundtrip lost on_unsupported: %q", reloaded.IngestOnUnsupported)
	}
}
