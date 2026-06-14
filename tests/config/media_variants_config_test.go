package tests

import (
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestMediaVariants_DefaultsDisabledSelectBest(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaVariantsGroup {
		t.Fatalf("media.variants.group must default to false")
	}
	if cfg.MediaVariantsSelect != "best" {
		t.Fatalf("media.variants.select default = %q, want best", cfg.MediaVariantsSelect)
	}
}

func TestMediaVariants_ValidateRejectsUnknownSelect(t *testing.T) {
	cfg := config.Default()
	cfg.MediaVariantsSelect = "smallest"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unknown media.variants.select")
	}
}

func TestMediaVariants_ValidateNormalizesSelect(t *testing.T) {
	cfg := config.Default()
	cfg.MediaVariantsSelect = "FIRST"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("first must be a valid select policy: %v", err)
	}
	if cfg.MediaVariantsSelect != "first" {
		t.Fatalf("media.variants.select should normalize to lowercase, got %q", cfg.MediaVariantsSelect)
	}
}

func TestMediaVariants_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaVariantsGroup = true
	cfg.MediaVariantsSelect = "first"
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if !loaded.MediaVariantsGroup {
		t.Fatalf("media.variants.group did not round-trip")
	}
	if loaded.MediaVariantsSelect != "first" {
		t.Fatalf("media.variants.select did not round-trip: got %q", loaded.MediaVariantsSelect)
	}
}
