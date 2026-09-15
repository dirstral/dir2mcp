package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaTranslateNameHints_DefaultsOffAndRoundTrips pins that proper-noun
// spelling hints are OFF by default (so existing prompts are unchanged), that
// the key binds from nested YAML and from the flat alias, and that it survives a
// save/load round-trip (SPEC §8.6.2, §16.2).
func TestMediaTranslateNameHints_DefaultsOffAndRoundTrips(t *testing.T) {
	if config.Default().MediaTranslateNameHints {
		t.Fatalf("media.translate.name_hints must default to false")
	}

	tmp := t.TempDir()
	for name, body := range map[string]string{
		"nested": "media:\n  translate:\n    name_hints: true\n",
		"flat":   "media_translate_name_hints: true\n",
	} {
		path := filepath.Join(tmp, name+".yaml")
		writeFile(t, path, strings.Join([]string{"root_dir: /tmp/repo", "state_dir: /tmp/repo/.dir2mcp"}, "\n")+"\n"+body)
		loaded, err := config.LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile(%s): %v", name, err)
		}
		if !loaded.MediaTranslateNameHints {
			t.Fatalf("%s media.translate.name_hints not applied", name)
		}
	}

	save := config.Default()
	save.RootDir, save.StateDir = "/tmp/repo", "/tmp/repo/.dir2mcp"
	save.MediaTranslateNameHints = true
	out := filepath.Join(tmp, "saved.yaml")
	if err := config.SaveFile(out, save); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	back, err := config.LoadFile(out)
	if err != nil {
		t.Fatalf("LoadFile(saved): %v", err)
	}
	if !back.MediaTranslateNameHints {
		t.Fatalf("name_hints did not round-trip")
	}
}
