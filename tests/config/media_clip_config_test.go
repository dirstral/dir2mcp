package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaClip_Defaults locks the built-in clip bounds (SPEC §15.11): a
// 2-minute span cap and a 25 MiB inline byte cap.
func TestMediaClip_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaClipMaxDurationMS != config.DefaultMediaClipMaxDurationMS {
		t.Errorf("media.clip.max_duration_ms default = %d, want %d", cfg.MediaClipMaxDurationMS, config.DefaultMediaClipMaxDurationMS)
	}
	if cfg.MediaClipMaxBytes != config.DefaultMediaClipMaxBytes {
		t.Errorf("media.clip.max_bytes default = %d, want %d", cfg.MediaClipMaxBytes, config.DefaultMediaClipMaxBytes)
	}
	if config.DefaultMediaClipMaxDurationMS != 120000 {
		t.Errorf("DefaultMediaClipMaxDurationMS = %d, want 120000", config.DefaultMediaClipMaxDurationMS)
	}
	if config.DefaultMediaClipMaxBytes != 26214400 {
		t.Errorf("DefaultMediaClipMaxBytes = %d, want 26214400", config.DefaultMediaClipMaxBytes)
	}
}

// TestMediaClip_RoundTripsThroughSaveLoad exercises the int plumbing
// (setIntFileScalar/intPtr, writeInt) for the clip bound scalars.
func TestMediaClip_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaClipMaxDurationMS = 30000
	cfg.MediaClipMaxBytes = 1048576
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if loaded.MediaClipMaxDurationMS != 30000 {
		t.Errorf("media.clip.max_duration_ms did not round-trip: got %d, want 30000", loaded.MediaClipMaxDurationMS)
	}
	if loaded.MediaClipMaxBytes != 1048576 {
		t.Errorf("media.clip.max_bytes did not round-trip: got %d, want 1048576", loaded.MediaClipMaxBytes)
	}
}

// TestMediaClip_NestedYAMLApplies locks the nested media.clip: block so the
// bound scalars are applied via the YAML scanner (isMapSectionKey("media.clip")).
func TestMediaClip_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  clip:",
		"    max_duration_ms: 45000",
		"    max_bytes: 2097152",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.clip) = %v, want nil", err)
	}
	if cfg.MediaClipMaxDurationMS != 45000 {
		t.Errorf("nested media.clip.max_duration_ms = %d, want 45000", cfg.MediaClipMaxDurationMS)
	}
	if cfg.MediaClipMaxBytes != 2097152 {
		t.Errorf("nested media.clip.max_bytes = %d, want 2097152", cfg.MediaClipMaxBytes)
	}
}

// TestMediaClip_RejectsNegative asserts negative clip bounds are rejected at
// config-parse time rather than silently clamped.
func TestMediaClip_RejectsNegative(t *testing.T) {
	for _, key := range []string{"media_clip_max_duration_ms", "media_clip_max_bytes"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			key + ": -1",
		}, "\n")+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with %s=-1 = nil error, want rejection", key)
		}
	}
}
