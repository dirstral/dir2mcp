package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaTranslateContext_Defaults locks the built-in chat-translate window
// defaults (issue #573): cues are translated in windows with a small read-only
// margin by default so the model gets cross-line context.
func TestMediaTranslateContext_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaTranslateWindowLines != config.DefaultMediaTranslateWindowLines {
		t.Errorf("media.translate.window_lines default = %d, want %d",
			cfg.MediaTranslateWindowLines, config.DefaultMediaTranslateWindowLines)
	}
	if cfg.MediaTranslateContextLines != config.DefaultMediaTranslateContextLines {
		t.Errorf("media.translate.context_lines default = %d, want %d",
			cfg.MediaTranslateContextLines, config.DefaultMediaTranslateContextLines)
	}
}

// TestMediaTranslateContext_RoundTripsThroughSaveLoad exercises the int plumbing
// (setIntFileScalar, writeInt) for the window/context scalars through save/load.
func TestMediaTranslateContext_RoundTripsThroughSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.MediaTranslateWindowLines = 24
	cfg.MediaTranslateContextLines = 5
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if loaded.MediaTranslateWindowLines != 24 {
		t.Errorf("media.translate.window_lines did not round-trip: got %d, want 24", loaded.MediaTranslateWindowLines)
	}
	if loaded.MediaTranslateContextLines != 5 {
		t.Errorf("media.translate.context_lines did not round-trip: got %d, want 5", loaded.MediaTranslateContextLines)
	}
}

// TestMediaTranslateContext_NestedYAMLApplies locks the nested media: translate:
// block for the new scalars.
func TestMediaTranslateContext_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"media:",
		"  translate:",
		"    window_lines: 12",
		"    context_lines: 2",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(nested media.translate) = %v, want nil", err)
	}
	if cfg.MediaTranslateWindowLines != 12 {
		t.Errorf("nested media.translate.window_lines = %d, want 12", cfg.MediaTranslateWindowLines)
	}
	if cfg.MediaTranslateContextLines != 2 {
		t.Errorf("nested media.translate.context_lines = %d, want 2", cfg.MediaTranslateContextLines)
	}
}

// TestMediaTranslateContext_RejectsNegative asserts a negative window/context
// value is rejected at config-parse time rather than silently clamped.
func TestMediaTranslateContext_RejectsNegative(t *testing.T) {
	for _, key := range []string{"media_translate_window_lines", "media_translate_context_lines"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, strings.Join([]string{
			"root_dir: /tmp/repo",
			"state_dir: /tmp/repo/.dir2mcp",
			key + ": -3",
		}, "\n")+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with %s=-3 = nil error, want rejection", key)
		}
	}
}
