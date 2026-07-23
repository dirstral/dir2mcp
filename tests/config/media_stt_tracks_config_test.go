package tests

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestParseSTTTracks_ResolvesForms covers the media.stt.tracks grammar (SPEC
// §8.6.12): the default/`first` keyword, `all`, and an explicit 0-based index list
// that is de-duplicated and sorted into container stream order.
func TestParseSTTTracks_ResolvesForms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     []string
		mode    config.STTTrackMode
		indices []int
	}{
		{"empty defaults to first", nil, config.STTTracksFirst, nil},
		{"first keyword", []string{"first"}, config.STTTracksFirst, nil},
		{"all keyword", []string{"all"}, config.STTTracksAll, nil},
		{"single index", []string{"0"}, config.STTTracksList, []int{0}},
		{"index list", []string{"0", "2"}, config.STTTracksList, []int{0, 2}},
		{"list sorted and deduped in stream order", []string{"2", "0"}, config.STTTracksList, []int{0, 2}},
		{"blank entries ignored", []string{" ", "1"}, config.STTTracksList, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := config.ParseSTTTracks(tc.raw)
			if err != nil {
				t.Fatalf("ParseSTTTracks(%v): %v", tc.raw, err)
			}
			if sel.Mode != tc.mode {
				t.Errorf("mode = %v, want %v", sel.Mode, tc.mode)
			}
			if !reflect.DeepEqual(sel.Indices, tc.indices) {
				t.Errorf("indices = %v, want %v", sel.Indices, tc.indices)
			}
		})
	}
}

// TestParseSTTTracks_RejectsInvalid confirms the CONFIG_INVALID cases: an unknown
// keyword, a keyword mixed with indices, a negative index, and a duplicate index.
func TestParseSTTTracks_RejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []string
	}{
		{"unknown keyword", []string{"middle"}},
		{"keyword mixed with index", []string{"all", "1"}},
		{"negative index", []string{"-1"}},
		{"duplicate index", []string{"0", "0"}},
		{"non-integer entry", []string{"0", "two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.ParseSTTTracks(tc.raw); err == nil {
				t.Fatalf("ParseSTTTracks(%v) = nil error, want CONFIG_INVALID", tc.raw)
			}
		})
	}
}

// TestMediaSTTTracks_DefaultEmpty confirms the selection is unset by default, so an
// unconfigured corpus transcribes only the first track (byte-for-byte unchanged).
func TestMediaSTTTracks_DefaultEmpty(t *testing.T) {
	cfg := config.Default()
	if len(cfg.MediaSTTTracks) != 0 {
		t.Errorf("media.stt.tracks default = %v, want empty", cfg.MediaSTTTracks)
	}
	sel, err := config.ParseSTTTracks(cfg.MediaSTTTracks)
	if err != nil {
		t.Fatalf("ParseSTTTracks(default): %v", err)
	}
	if sel.Mode != config.STTTracksFirst {
		t.Errorf("default selection mode = %v, want first", sel.Mode)
	}
}

// TestMediaSTTTracks_ParsesYAMLForms exercises the loader for the scalar keyword,
// the inline list, and the block list forms.
func TestMediaSTTTracks_ParsesYAMLForms(t *testing.T) {
	base := []string{"root_dir: /tmp/repo", "state_dir: /tmp/repo/.dir2mcp"}
	cases := map[string]struct {
		lines []string
		want  []string
	}{
		"scalar all": {
			lines: append(append([]string(nil), base...), "media:", "  stt:", "    tracks: all"),
			want:  []string{"all"},
		},
		"flat scalar first": {
			lines: append(append([]string(nil), base...), "media_stt_tracks: first"),
			want:  []string{"first"},
		},
		"inline list": {
			lines: append(append([]string(nil), base...), "media:", "  stt:", "    tracks: [0, 2]"),
			want:  []string{"0", "2"},
		},
		"block list": {
			lines: append(append([]string(nil), base...), "media:", "  stt:", "    tracks:", "      - 0", "      - 2"),
			want:  []string{"0", "2"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(tc.lines, "\n")+"\n")
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile(%s): %v", name, err)
			}
			if !reflect.DeepEqual(cfg.MediaSTTTracks, tc.want) {
				t.Errorf("%s: media.stt.tracks = %v, want %v", name, cfg.MediaSTTTracks, tc.want)
			}
		})
	}
}

// TestMediaSTTTracks_RoundTripsThroughSaveLoad exercises the []string plumbing
// (setFileListValue / writeList) end to end.
func TestMediaSTTTracks_RoundTripsThroughSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaSTTTracks = []string{"0", "2"}
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(loaded.MediaSTTTracks, []string{"0", "2"}) {
		t.Errorf("media.stt.tracks did not round-trip: got %v, want [0 2]", loaded.MediaSTTTracks)
	}
}

// TestMediaSTTTracks_ValidateRejectsInvalid confirms an invalid selection fails
// config validation (CONFIG_INVALID at startup, not per file).
func TestMediaSTTTracks_ValidateRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tracks []string
	}{
		{"duplicate", []string{"1", "1"}},
		{"negative", []string{"-2"}},
		{"unknown keyword", []string{"second"}},
		{"mixed", []string{"all", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MediaSTTTracks = tc.tracks
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want CONFIG_INVALID for %v", tc.tracks)
			}
		})
	}
}
