package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaWindow_Defaults locks the unset defaults to zero so the ingest path
// falls back to the built-in window constants (audio 120 s, video 60 s),
// keeping behavior identical when unconfigured (SPEC 8.1.7).
func TestMediaWindow_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaAudioWindowSec != 0 {
		t.Errorf("media.audio_window_sec default = %d, want 0 (constant fallback)", cfg.MediaAudioWindowSec)
	}
	if cfg.MediaVideoWindowSec != 0 {
		t.Errorf("media.video_window_sec default = %d, want 0 (constant fallback)", cfg.MediaVideoWindowSec)
	}
}

// TestMediaWindow_RoundTripsThroughSaveLoad exercises the int plumbing
// (setIntFileScalar/intPtr, writeInt) for the new window scalars through
// save/load.
func TestMediaWindow_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaAudioWindowSec = 90
	cfg.MediaVideoWindowSec = 45
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if loaded.MediaAudioWindowSec != 90 {
		t.Errorf("media.audio_window_sec did not round-trip: got %d, want 90", loaded.MediaAudioWindowSec)
	}
	if loaded.MediaVideoWindowSec != 45 {
		t.Errorf("media.video_window_sec did not round-trip: got %d, want 45", loaded.MediaVideoWindowSec)
	}
}

// TestMediaWindow_NestedYAMLApplies locks the nested media: block so the window
// scalars are applied via the bespoke YAML scanner (isMapSectionKey("media")).
func TestMediaWindow_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  audio_window_sec: 150",
		"  video_window_sec: 100",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media) = %v, want nil", err)
	}
	if cfg.MediaAudioWindowSec != 150 {
		t.Errorf("nested media.audio_window_sec = %d, want 150", cfg.MediaAudioWindowSec)
	}
	if cfg.MediaVideoWindowSec != 100 {
		t.Errorf("nested media.video_window_sec = %d, want 100", cfg.MediaVideoWindowSec)
	}
}

// TestMediaWindow_RejectsNegative asserts a negative window value is rejected at
// config-parse time (explicit, deterministic) rather than silently clamped.
func TestMediaWindow_RejectsNegative(t *testing.T) {
	for _, key := range []string{"media_audio_window_sec", "media_video_window_sec"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			key + ": -5",
		}, "\n")+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with %s=-5 = nil error, want rejection", key)
		}
	}
}

// TestMediaWindow_RejectsNonInteger asserts a non-integer window value is
// rejected at config-parse time.
func TestMediaWindow_RejectsNonInteger(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media_audio_window_sec: abc",
	}, "\n")+"\n")

	if _, err := config.LoadFile(path); err == nil {
		t.Error("LoadFile with media_audio_window_sec=abc = nil error, want rejection")
	}
}
