package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §8.6.3 / §16.2 (0.87.0): media.subtitles.source_pause_ms.

// TestSourcePauseMS_Default pins the spec default of 2000 ms, and that the
// default config validates.
func TestSourcePauseMS_Default(t *testing.T) {
	cfg := config.Default()
	if config.DefaultMediaSubtitlesSourcePauseMS != 2000 {
		t.Fatalf("DefaultMediaSubtitlesSourcePauseMS = %d, want 2000", config.DefaultMediaSubtitlesSourcePauseMS)
	}
	if cfg.MediaSubtitlesSourcePauseMS != 2000 {
		t.Fatalf("media.subtitles.source_pause_ms default = %d, want 2000", cfg.MediaSubtitlesSourcePauseMS)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default: %v", err)
	}
	if cfg.MediaSubtitlesSourcePauseMS != 2000 {
		t.Fatalf("Validate changed the default to %d", cfg.MediaSubtitlesSourcePauseMS)
	}
}

// TestSourcePauseMS_NegativeIsConfigInvalid pins that a negative value fails
// validation with CONFIG_INVALID, and that 0 (off) is kept as 0.
func TestSourcePauseMS_NegativeIsConfigInvalid(t *testing.T) {
	bad := config.Default()
	bad.MediaSubtitlesSourcePauseMS = -1
	err := bad.Validate()
	if err == nil {
		t.Fatal("negative media.subtitles.source_pause_ms should be rejected")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "source_pause_ms") {
		t.Fatalf("error %q should name CONFIG_INVALID and source_pause_ms", err)
	}
	off := config.Default()
	off.MediaSubtitlesSourcePauseMS = 0
	if err := off.Validate(); err != nil {
		t.Fatalf("source_pause_ms 0 should validate: %v", err)
	}
	if off.MediaSubtitlesSourcePauseMS != 0 {
		t.Fatalf("source_pause_ms 0 became %d after Validate; 0 means off", off.MediaSubtitlesSourcePauseMS)
	}
}

// TestSourcePauseMS_FileValues pins the nested YAML key: an integer applies,
// an explicit 0 stays 0, and a negative or non-integer value fails the load.
func TestSourcePauseMS_FileValues(t *testing.T) {
	load := func(value string) (config.Config, error) {
		path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
		writeFile(t, path, strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			"media:",
			"  subtitles:",
			"    source_pause_ms: " + value,
		}, "\n")+"\n")
		return config.LoadFile(path)
	}
	for _, tc := range []struct {
		value string
		want  int
	}{{"1500", 1500}, {"0", 0}} {
		cfg, err := load(tc.value)
		if err != nil {
			t.Fatalf("LoadFile(source_pause_ms: %s): %v", tc.value, err)
		}
		if cfg.MediaSubtitlesSourcePauseMS != tc.want {
			t.Fatalf("source_pause_ms: %s loaded as %d, want %d", tc.value, cfg.MediaSubtitlesSourcePauseMS, tc.want)
		}
	}
	for _, value := range []string{"-5", "2.5", "two"} {
		if _, err := load(value); err == nil {
			t.Fatalf("source_pause_ms: %s should fail the load", value)
		}
	}
}

// TestSourcePauseMS_SnapshotRoundTrip pins that the config snapshot persists
// the key, including 0 (off), which must not fall back to the default.
func TestSourcePauseMS_SnapshotRoundTrip(t *testing.T) {
	for _, want := range []int{3500, 0} {
		path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
		cfg := config.Default()
		cfg.RootDir = "/tmp/repo"
		cfg.StateDir = "/tmp/repo/.dir2mcp"
		cfg.MediaSubtitlesSourcePauseMS = want
		if err := config.SaveFile(path, cfg); err != nil {
			t.Fatalf("SaveFile: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		if !strings.Contains(string(raw), "source_pause_ms") {
			t.Fatalf("snapshot does not hold source_pause_ms:\n%s", raw)
		}
		loaded, err := config.LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		if loaded.MediaSubtitlesSourcePauseMS != want {
			t.Fatalf("source_pause_ms did not round-trip: got %d, want %d", loaded.MediaSubtitlesSourcePauseMS, want)
		}
	}
}
