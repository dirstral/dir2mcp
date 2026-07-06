package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaSTTLimits_DefaultsAreZero confirms the STT request-limit knobs default
// to 0 (= use the whisper client's built-in caps), so behavior is unchanged when
// unset (dir2mcp#510/#511).
func TestMediaSTTLimits_DefaultsAreZero(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaSTTMaxPayloadMB != 0 {
		t.Errorf("media.stt.max_payload_mb default = %d, want 0", cfg.MediaSTTMaxPayloadMB)
	}
	if cfg.MediaSTTRequestTimeoutSec != 0 {
		t.Errorf("media.stt.request_timeout_sec default = %d, want 0", cfg.MediaSTTRequestTimeoutSec)
	}
}

// TestMediaSTTLimits_RoundTripsThroughSaveLoad exercises the int plumbing
// (setIntFileScalar/intPtr, writeInt) for the two scalars.
func TestMediaSTTLimits_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaSTTMaxPayloadMB = 250
	cfg.MediaSTTRequestTimeoutSec = 3600
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaSTTMaxPayloadMB != 250 {
		t.Errorf("max_payload_mb did not round-trip: got %d, want 250", loaded.MediaSTTMaxPayloadMB)
	}
	if loaded.MediaSTTRequestTimeoutSec != 3600 {
		t.Errorf("request_timeout_sec did not round-trip: got %d, want 3600", loaded.MediaSTTRequestTimeoutSec)
	}
}

// TestMediaSTTLimits_ParsesFlatAndNestedYAML confirms both the flat keys
// (media_stt_*) and the nested block (media: stt: ...) apply through the loader.
func TestMediaSTTLimits_ParsesFlatAndNestedYAML(t *testing.T) {
	base := []string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
	}
	flat := append(append([]string(nil), base...),
		"media_stt_max_payload_mb: 300",
		"media_stt_request_timeout_sec: 900")
	nested := append(append([]string(nil), base...),
		"media:", "  stt:", "    max_payload_mb: 300", "    request_timeout_sec: 900")

	for name, lines := range map[string][]string{"flat": flat, "nested": nested} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(lines, "\n")+"\n")
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile(%s media.stt): %v", name, err)
			}
			if cfg.MediaSTTMaxPayloadMB != 300 {
				t.Errorf("%s max_payload_mb = %d, want 300", name, cfg.MediaSTTMaxPayloadMB)
			}
			if cfg.MediaSTTRequestTimeoutSec != 900 {
				t.Errorf("%s request_timeout_sec = %d, want 900", name, cfg.MediaSTTRequestTimeoutSec)
			}
		})
	}
}

// TestMediaSTTLimits_NegativeRejected confirms negative values are CONFIG_INVALID.
func TestMediaSTTLimits_NegativeRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
	}{
		{"negative payload", func(c *config.Config) { c.MediaSTTMaxPayloadMB = -1 }},
		{"negative timeout", func(c *config.Config) { c.MediaSTTRequestTimeoutSec = -5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.set(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}
