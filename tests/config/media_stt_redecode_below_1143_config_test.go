package tests

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §8.6.13 (spec 0.88.0, dir2mcp #1143): media.stt.redecode_below is the
// confidence below which a window decoded in another language than the item's
// is decoded again. Default 0.5; 0 turns the rule off; a value outside [0,1]
// is CONFIG_INVALID.

func TestRedecodeBelow_DefaultIsHalf(t *testing.T) {
	if got := config.Default().MediaSTTRedecodeBelow; got != 0.5 {
		t.Fatalf("default media.stt.redecode_below = %v, want 0.5", got)
	}
}

func TestRedecodeBelow_Validation(t *testing.T) {
	for _, tc := range []struct {
		value   float64
		wantErr bool
	}{
		{value: 0}, {value: 0.5}, {value: 1},
		{value: -0.1, wantErr: true}, {value: 1.5, wantErr: true}, {value: math.NaN(), wantErr: true},
	} {
		cfg := config.Default()
		cfg.MediaSTTRedecodeBelow = tc.value
		err := cfg.Validate()
		if !tc.wantErr {
			if err != nil {
				t.Errorf("redecode_below=%v: %v, want valid", tc.value, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "redecode_below") {
			t.Errorf("redecode_below=%v: err = %v, want CONFIG_INVALID naming media.stt.redecode_below", tc.value, err)
		}
	}
}

func TestRedecodeBelow_ParsesFlatAndNestedYAML(t *testing.T) {
	base := "root_dir: /tmp/repo\nstate_dir: /tmp/repo/.dir2mcp\n"
	for name, body := range map[string]string{
		"flat":   base + "media_stt_redecode_below: 0.3\n",
		"nested": base + "media:\n  stt:\n    redecode_below: 0.3\n",
		"off":    base + "media:\n  stt:\n    redecode_below: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			want := 0.3
			if name == "off" {
				want = 0
			}
			if cfg.MediaSTTRedecodeBelow != want {
				t.Errorf("redecode_below = %v, want %v", cfg.MediaSTTRedecodeBelow, want)
			}
		})
	}
}

func TestRedecodeBelow_OutOfRangeInFileIsConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: /tmp/repo\nstate_dir: /tmp/repo/.dir2mcp\nmedia:\n  stt:\n    redecode_below: 1.5\n")
	_, err := config.LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("err = %v, want CONFIG_INVALID", err)
	}
}

func TestRedecodeBelow_RoundTripsThroughSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaSTTRedecodeBelow = 0.25
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if text := readFileString(t, path); !strings.Contains(text, "media_stt_redecode_below: 0.25") {
		t.Fatalf("saved config missing media_stt_redecode_below:\n%s", text)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.MediaSTTRedecodeBelow != 0.25 {
		t.Fatalf("redecode_below = %v after a round trip, want 0.25", loaded.MediaSTTRedecodeBelow)
	}
}
