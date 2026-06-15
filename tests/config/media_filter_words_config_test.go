package tests

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaFilterWords_DefaultsEmpty pins the off-by-default contract: there are
// NO built-in filter phrases, so the feature is inactive unless configured.
func TestMediaFilterWords_DefaultsEmpty(t *testing.T) {
	cfg := config.Default()
	if len(cfg.MediaFilterWords) != 0 {
		t.Fatalf("media.filter_words must default to empty, got %#v", cfg.MediaFilterWords)
	}
}

// TestMediaFilterWords_RoundTripsThroughSaveLoad pins persistence of the list
// through SaveFile/LoadFile (flat-key YAML).
func TestMediaFilterWords_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaFilterWords = []string{"Subscribe to our channel", "watermark"}
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if !reflect.DeepEqual(loaded.MediaFilterWords, cfg.MediaFilterWords) {
		t.Fatalf("media.filter_words did not round-trip: got %#v want %#v",
			loaded.MediaFilterWords, cfg.MediaFilterWords)
	}
}

// TestMediaFilterWords_NestedYAMLApplies pins the nested spec-style block
// (media: -> filter_words: -> list items) is applied (isMapSectionKey + list-key
// handling).
func TestMediaFilterWords_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  filter_words:",
		"    - Subscribe to our channel",
		"    - watermark",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.filter_words) = %v, want nil", err)
	}
	want := []string{"Subscribe to our channel", "watermark"}
	if !reflect.DeepEqual(cfg.MediaFilterWords, want) {
		t.Fatalf("nested media.filter_words not applied: got %#v want %#v", cfg.MediaFilterWords, want)
	}
}

// TestMediaFilterWords_FlatAliasApplies pins that the flat alias key (and the
// short `filter_words` alias) resolve to media.filter_words.
func TestMediaFilterWords_FlatAliasApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"filter_words:",
		"  - boilerplate",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(flat filter_words) = %v, want nil", err)
	}
	if !reflect.DeepEqual(cfg.MediaFilterWords, []string{"boilerplate"}) {
		t.Fatalf("flat filter_words alias not applied: got %#v", cfg.MediaFilterWords)
	}
}
