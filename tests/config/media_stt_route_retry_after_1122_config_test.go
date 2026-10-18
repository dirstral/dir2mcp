package tests

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): media.stt.route_retry_after is a
// duration, default 24h; 0 makes a fallback result due on every run; a
// negative value or one that is not a duration is CONFIG_INVALID. Env
// DIR2MCP_MEDIA_STT_ROUTE_RETRY_AFTER.

func TestRouteRetryAfter_DefaultIs24h(t *testing.T) {
	if got := config.Default().MediaSTTRouteRetryAfter; got != 24*time.Hour {
		t.Fatalf("default = %v, want 24h", got)
	}
	cfg := loadCfg(t, whisperProfiles)
	if err := cfg.Validate(); err != nil || cfg.MediaSTTRouteRetryAfter != 24*time.Hour {
		t.Fatalf("loaded default = %v (%v), want 24h", cfg.MediaSTTRouteRetryAfter, err)
	}
}

func TestRouteRetryAfter_FileValues(t *testing.T) {
	for value, want := range map[string]time.Duration{"0": 0, "24h": 24 * time.Hour, "90m": 90 * time.Minute} {
		t.Run(value, func(t *testing.T) {
			cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    route_retry_after: "+value+"\n")
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if cfg.MediaSTTRouteRetryAfter != want {
				t.Errorf("route_retry_after = %v, want %v", cfg.MediaSTTRouteRetryAfter, want)
			}
		})
	}
}

func TestRouteRetryAfter_NegativeIsConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, whisperProfiles+"media:\n  stt:\n    route_retry_after: -1h\n")
	_, loadErr := config.LoadFile(path)
	cfg := config.Default()
	cfg.MediaSTTRouteRetryAfter = -time.Hour
	for name, err := range map[string]error{"load": loadErr, "validate": cfg.Validate()} {
		if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "route_retry_after") {
			t.Errorf("%s: err = %v, want CONFIG_INVALID naming media.stt.route_retry_after", name, err)
		}
	}
}

func TestRouteRetryAfter_NotADurationIsConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, whisperProfiles+"media:\n  stt:\n    route_retry_after: abc\n")
	_, err := config.LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "route_retry_after") {
		t.Fatalf("err = %v, want CONFIG_INVALID naming media.stt.route_retry_after", err)
	}
}

func TestRouteRetryAfter_Env(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "root_dir: ./repo\nmedia:\n  stt:\n    route_retry_after: 24h\n")
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_MEDIA_STT_ROUTE_RETRY_AFTER", " 0 ")
		cfg, err := config.Load(".dir2mcp.yaml")
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil || cfg.MediaSTTRouteRetryAfter != 0 {
			t.Fatalf("env 0: route_retry_after = %v (%v), want 0", cfg.MediaSTTRouteRetryAfter, err)
		}
		for _, bad := range []string{"abc", "-1h"} {
			t.Setenv("DIR2MCP_MEDIA_STT_ROUTE_RETRY_AFTER", bad)
			cfg, err = config.Load(".dir2mcp.yaml")
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
				t.Errorf("env %q: err = %v, want CONFIG_INVALID", bad, err)
			}
		}
	})
}

func TestRouteRetryAfter_SnapshotRoundTrip(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    route_retry_after: 90m\n")
	cfg.StateDir = t.TempDir()
	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if loaded.MediaSTTRouteRetryAfter != 90*time.Minute {
		t.Errorf("snapshot route_retry_after = %v, want 90m", loaded.MediaSTTRouteRetryAfter)
	}
}
