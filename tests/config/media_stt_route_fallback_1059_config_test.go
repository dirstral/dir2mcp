package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §8.2.4 (dir2mcp #1059): media.stt.on_route_error selects the route
// fallback, and a language_providers key must be a BCP-47 language tag.

func TestOnRouteError_ParsesAndDefaultsToFail(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    on_route_error: default\n")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !cfg.RouteErrorFallsBack() {
		t.Errorf("on_route_error: default must select the fallback")
	}
	def := config.Default()
	if def.MediaSTTOnRouteError != "fail" || def.RouteErrorFallsBack() {
		t.Errorf("default policy = %q (falls back %v), want fail", def.MediaSTTOnRouteError, def.RouteErrorFallsBack())
	}
	upper := loadCfg(t, whisperProfiles+"media:\n  stt:\n    on_route_error: FAIL\n")
	if err := upper.Validate(); err != nil || upper.MediaSTTOnRouteError != "fail" {
		t.Errorf("FAIL must normalize to fail: %q (%v)", upper.MediaSTTOnRouteError, err)
	}
}

func TestOnRouteError_RejectsUnknownValue(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSTTOnRouteError = "retry"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "on_route_error") {
		t.Fatalf("err = %v, want a CONFIG_INVALID naming media.stt.on_route_error", err)
	}
}

func TestOnRouteError_SnapshotRoundTrip(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    on_route_error: default\n")
	cfg.StateDir = t.TempDir()
	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if loaded.MediaSTTOnRouteError != "default" || !loaded.RouteErrorFallsBack() {
		t.Errorf("snapshot on_route_error = %q, want default", loaded.MediaSTTOnRouteError)
	}
}

// TestLanguageProviders_KeyMustBeALanguageTag: a key that can never match a
// resolved language is a configuration error at load, not a route that never
// fires (SPEC §8.2.4).
func TestLanguageProviders_KeyMustBeALanguageTag(t *testing.T) {
	for _, key := range []string{"fa_IR", "f", "abcdefghi", "k1", "fa.ir"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, whisperProfiles+"media:\n  stt:\n    language_providers:\n      \""+key+"\": a\n")
			_, err := config.LoadFile(path)
			if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "language_providers key") {
				t.Fatalf("key %q: err = %v, want CONFIG_INVALID naming the key", key, err)
			}
		})
	}
	ok := loadCfg(t, whisperProfiles+"media:\n  stt:\n    language_providers:\n      pt-BR: a\n      ky: b\n      und: a\n      persian: b\n")
	// "persian" is 7 letters: a well-formed (registered-length) primary subtag
	// by shape, which is all the check asserts; no language list ships.
	if ok.MediaSTTLanguageProviders["pt"] != "a" || ok.MediaSTTLanguageProviders["ky"] != "b" || ok.MediaSTTLanguageProviders["und"] != "a" || ok.MediaSTTLanguageProviders["persian"] != "b" {
		t.Errorf("well-formed keys must load on their primary subtag: %v", ok.MediaSTTLanguageProviders)
	}
}
