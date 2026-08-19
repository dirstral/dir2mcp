package tests

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// Config guards for #894. The bound on one /recognize call used to be a
// hard-coded ten minutes with no config key at all, so vision recognition could
// not run on long media.

// TestRecognizeTimeout_Defaults pins the shipped defaults, so an operator learns
// them from a test rather than from the source.
func TestRecognizeTimeout_Defaults(t *testing.T) {
	cfg := config.Default()
	if want := 10 * time.Minute; cfg.RecognizeTimeout != want {
		t.Errorf("RecognizeTimeout default = %s, want %s", cfg.RecognizeTimeout, want)
	}
	if want := 2.0; cfg.RecognizeTimeoutPerMediaSecond != want {
		t.Errorf("RecognizeTimeoutPerMediaSecond default = %v, want %v",
			cfg.RecognizeTimeoutPerMediaSecond, want)
	}
	// The constants and the defaults must not drift apart.
	if cfg.RecognizeTimeout != config.DefaultRecognizeTimeout {
		t.Errorf("Default() disagrees with DefaultRecognizeTimeout")
	}
	if cfg.RecognizeTimeoutPerMediaSecond != config.DefaultRecognizeTimeoutPerMediaSecond {
		t.Errorf("Default() disagrees with DefaultRecognizeTimeoutPerMediaSecond")
	}
}

// TestRecognizeTimeout_KeysLoad pins both spellings of both keys: the dotted
// canonical form and the underscore form the snapshot writer emits (#624).
func TestRecognizeTimeout_KeysLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"dotted", "recognize.timeout: 90m\nrecognize.timeout_per_media_second: 3.5\n"},
		{"underscore", "recognize_timeout: 90m\nrecognize_timeout_per_media_second: 3.5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, tc.body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if want := 90 * time.Minute; cfg.RecognizeTimeout != want {
				t.Errorf("RecognizeTimeout = %s, want %s", cfg.RecognizeTimeout, want)
			}
			if want := 3.5; cfg.RecognizeTimeoutPerMediaSecond != want {
				t.Errorf("RecognizeTimeoutPerMediaSecond = %v, want %v",
					cfg.RecognizeTimeoutPerMediaSecond, want)
			}
		})
	}
}

// TestRecognizeTimeout_NegativeIsConfigInvalid pins that a bound the daemon cannot
// honour fails at startup instead of being silently coerced.
func TestRecognizeTimeout_NegativeIsConfigInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		mut  func(*config.Config)
	}{
		{"negative timeout", "recognize.timeout", func(c *config.Config) { c.RecognizeTimeout = -time.Second }},
		{"negative ratio", "recognize.timeout_per_media_second",
			func(c *config.Config) { c.RecognizeTimeoutPerMediaSecond = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.RootDir = t.TempDir()
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected CONFIG_INVALID for %s", tc.want)
			}
			if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a CONFIG_INVALID naming %s", err, tc.want)
			}
		})
	}
}

// TestRecognizeTimeout_ZeroIsLegal pins that zero is a valid value for both keys.
// Zero means "this half does not constrain the call": zero on the ratio disables
// the duration scaling, and zero on both falls back to the shipped default rather
// than leaving a call unbounded.
func TestRecognizeTimeout_ZeroIsLegal(t *testing.T) {
	cfg := config.Default()
	cfg.RootDir = t.TempDir()
	cfg.RecognizeTimeout = 0
	cfg.RecognizeTimeoutPerMediaSecond = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero bounds must be legal, got %v", err)
	}
}
