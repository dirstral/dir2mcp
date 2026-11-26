package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §8.10.1 (spec 0.103.0): the media.shots keys, their defaults and their
// CONFIG_INVALID rules.

func TestMediaShots0103_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.ShotsDetector != "off" || cfg.ShotsThreshold != 0.2 || cfg.ShotsMinShotS != 0.5 ||
		cfg.ShotsTimeout != 10*time.Minute || cfg.ShotsTimeoutPerMediaSecond != 1.0 || cfg.ShotsMaxKeyframeGapS != 60 {
		t.Fatalf("defaults = %q %v %v %v %v %v", cfg.ShotsDetector, cfg.ShotsThreshold, cfg.ShotsMinShotS,
			cfg.ShotsTimeout, cfg.ShotsTimeoutPerMediaSecond, cfg.ShotsMaxKeyframeGapS)
	}
	cfg.RootDir = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}
}

func TestMediaShots0103_KeysLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"nested", "media:\n  shots:\n    detector: serve\n    threshold: 0.3\n    min_shot_s: 1.5\n" +
			"    base_url: http://127.0.0.1:8766\n    timeout: 20m\n    timeout_per_media_second: 0.25\n    max_keyframe_gap_s: 30\n"},
		{"dotted", "media.shots.detector: serve\nmedia.shots.threshold: 0.3\nmedia.shots.min_shot_s: 1.5\n" +
			"media.shots.base_url: http://127.0.0.1:8766\nmedia.shots.timeout: 20m\n" +
			"media.shots.timeout_per_media_second: 0.25\nmedia.shots.max_keyframe_gap_s: 30\n"},
		{"underscore", "media_shots_detector: serve\nmedia_shots_threshold: 0.3\nmedia_shots_min_shot_s: 1.5\n" +
			"media_shots_base_url: http://127.0.0.1:8766\nmedia_shots_timeout: 20m\n" +
			"media_shots_timeout_per_media_second: 0.25\nmedia_shots_max_keyframe_gap_s: 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, tc.body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if cfg.ShotsDetector != "serve" || cfg.ShotsThreshold != 0.3 || cfg.ShotsMinShotS != 1.5 ||
				cfg.ShotsBaseURL != "http://127.0.0.1:8766" || cfg.ShotsTimeout != 20*time.Minute ||
				cfg.ShotsTimeoutPerMediaSecond != 0.25 || cfg.ShotsMaxKeyframeGapS != 30 {
				t.Fatalf("loaded = %+v", []any{cfg.ShotsDetector, cfg.ShotsThreshold, cfg.ShotsMinShotS,
					cfg.ShotsBaseURL, cfg.ShotsTimeout, cfg.ShotsTimeoutPerMediaSecond, cfg.ShotsMaxKeyframeGapS})
			}
		})
	}
}

func TestMediaShots0103_ConfigInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		mut  func(*config.Config)
	}{
		{"unknown detector", "media.shots.detector", func(c *config.Config) { c.ShotsDetector = "transnet" }},
		{"serve without url", "media.shots.base_url", func(c *config.Config) { c.ShotsDetector = "serve" }},
		{"serve with credentials", "must not embed credentials", func(c *config.Config) {
			c.ShotsDetector = "serve"
			c.ShotsBaseURL = "http://user:secret@127.0.0.1:8766"
		}},
		{"threshold zero", "media.shots.threshold", func(c *config.Config) { c.ShotsThreshold = 0 }},
		{"threshold above one", "media.shots.threshold", func(c *config.Config) { c.ShotsThreshold = 1.5 }},
		{"negative min shot", "media.shots.min_shot_s", func(c *config.Config) { c.ShotsMinShotS = -1 }},
		{"negative timeout", "media.shots.timeout", func(c *config.Config) { c.ShotsTimeout = -time.Second }},
		{"keyframe gap below one", "media.shots.max_keyframe_gap_s", func(c *config.Config) { c.ShotsMaxKeyframeGapS = 0.5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.RootDir = t.TempDir()
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected CONFIG_INVALID naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a CONFIG_INVALID naming %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the error repeats the URL credentials: %v", err)
			}
		})
	}
}

// TestMediaShots0103_SnapshotRoundTrips: the snapshot writer's flat spellings
// must load back, or a generated .dir2mcp.yaml silently turns shots off (#624).
func TestMediaShots0103_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.ShotsDetector = "ffmpeg"
	cfg.ShotsThreshold = 0.25
	cfg.ShotsMaxKeyframeGapS = 45
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.ShotsDetector != "ffmpeg" || got.ShotsThreshold != 0.25 || got.ShotsMaxKeyframeGapS != 45 {
		t.Fatalf("round trip lost the shots keys: %q %v %v", got.ShotsDetector, got.ShotsThreshold, got.ShotsMaxKeyframeGapS)
	}
}

// TestMediaShots0103_PersistedURLDropsTheQuery: a token in the base_url query
// works at runtime but must not be written to a config file or snapshot.
func TestMediaShots0103_PersistedURLDropsTheQuery(t *testing.T) {
	cfg := config.Default()
	cfg.ShotsDetector = "serve"
	cfg.ShotsBaseURL = "http://127.0.0.1:8766/api?token=SECRET123#frag"
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET123") {
		t.Fatalf("the saved config stores the query token:\n%s", raw)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.ShotsBaseURL != "http://127.0.0.1:8766/api" {
		t.Fatalf("persisted base_url = %q, want the host and path only", got.ShotsBaseURL)
	}
}
