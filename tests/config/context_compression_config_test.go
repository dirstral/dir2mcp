package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestContextCompression_DefaultDisabled pins the local-first default: context
// compression is off and the target ratio is 0 (meaning "use the built-in
// default") unless explicitly configured.
func TestContextCompression_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.ContextCompressionEnabled {
		t.Fatalf("context compression must default to disabled")
	}
	if cfg.ContextCompressionTargetRatio != 0 {
		t.Fatalf("context_compression.target_ratio must default to 0, got %v", cfg.ContextCompressionTargetRatio)
	}
}

// TestContextCompression_NestedYAMLRoundTrips pins the nested-mapping form onto
// the Config fields.
func TestContextCompression_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  context_compression:",
		"    enabled: true",
		"    target_ratio: 0.4",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.ContextCompressionEnabled {
		t.Fatalf("nested retrieval.context_compression.enabled must parse to true")
	}
	if cfg.ContextCompressionTargetRatio != 0.4 {
		t.Fatalf("nested target_ratio = %v, want 0.4", cfg.ContextCompressionTargetRatio)
	}
}

// TestContextCompression_FlatAliasRoundTrips pins the flat snake_case aliases.
func TestContextCompression_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"context_compression_enabled: true",
		"context_compression_target_ratio: 0.6",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.ContextCompressionEnabled {
		t.Fatalf("flat context_compression_enabled must parse to true")
	}
	if cfg.ContextCompressionTargetRatio != 0.6 {
		t.Fatalf("flat context_compression_target_ratio = %v, want 0.6", cfg.ContextCompressionTargetRatio)
	}
}

// TestContextCompression_ShortAliasEnables pins the convenience alias
// `context_compression: true` onto the enable flag.
func TestContextCompression_ShortAliasEnables(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "context_compression: true\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.ContextCompressionEnabled {
		t.Fatalf("context_compression: true must enable compression")
	}
}

// TestContextCompression_SnapshotRoundTrips pins the persisted-snapshot
// round-trip: both knobs survive SaveEffectiveSnapshot -> YAML -> load.
func TestContextCompression_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.ContextCompressionEnabled = true
	cfg.ContextCompressionTargetRatio = 0.35

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "context_compression_enabled: true") {
		t.Fatalf("snapshot must persist context_compression_enabled: true:\n%s", raw)
	}
	if !strings.Contains(string(raw), "context_compression_target_ratio: 0.35") {
		t.Fatalf("snapshot must persist context_compression_target_ratio: 0.35:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.ContextCompressionEnabled || loaded.ContextCompressionTargetRatio != 0.35 {
		t.Fatalf("loaded snapshot mismatch: enabled=%v ratio=%v",
			loaded.ContextCompressionEnabled, loaded.ContextCompressionTargetRatio)
	}
}

// TestContextCompression_RejectsRatioOutOfRange pins validation: a ratio above 1
// or below 0 is CONFIG_INVALID.
func TestContextCompression_RejectsRatioOutOfRange(t *testing.T) {
	for _, bad := range []string{"1.5", "-0.2"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "context_compression_target_ratio: "+bad+"\n")

		_, err := config.LoadFile(path)
		if err == nil {
			t.Errorf("LoadFile with target_ratio=%q = nil error, want rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "context_compression.target_ratio") {
			t.Errorf("error for %q must mention context_compression.target_ratio, got: %v", bad, err)
		}
	}
}

// TestContextCompression_RejectsNonFiniteRatio pins that NaN/Inf strings (which
// strconv.ParseFloat accepts) are rejected at parse time.
func TestContextCompression_RejectsNonFiniteRatio(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf", "Infinity"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "context_compression_target_ratio: "+bad+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with target_ratio=%q = nil error, want rejection", bad)
		}
	}
}

// TestContextCompression_ValidateRejectsRatioProgrammatic pins that Validate()
// rejects an out-of-range ratio injected programmatically (not via the parser).
func TestContextCompression_ValidateRejectsRatioProgrammatic(t *testing.T) {
	cfg := config.Default()
	cfg.ContextCompressionTargetRatio = 2
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate with out-of-range ratio = nil error, want rejection")
	}
}
