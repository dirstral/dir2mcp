package setupwizard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
)

// TestProfileSettingsYAML_LoadsBack pins that the lines config init prints for
// an existing file, instead of rewriting it, are YAML the loader reads back to
// the profile's values, multi-line system prompt included.
func TestProfileSettingsYAML_LoadsBack(t *testing.T) {
	for _, profile := range []setupwizard.Profile{setupwizard.ProfileLegal, setupwizard.ProfileCode} {
		t.Run(string(profile), func(t *testing.T) {
			before := config.Default()
			after := before
			setupwizard.ApplyCorpusProfile(&after, profile)
			lines := setupwizard.ProfileSettingsYAML(before, after)
			if len(lines) == 0 || lines[0] != "rag:" {
				t.Fatalf("lines = %q, want a rag: block", lines)
			}
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile of the printed lines: %v\n%s", err, strings.Join(lines, "\n"))
			}
			if got.RAGKDefault != after.RAGKDefault || got.RAGMaxContextChars != after.RAGMaxContextChars {
				t.Errorf("k_default=%d max_context_chars=%d, want %d and %d", got.RAGKDefault, got.RAGMaxContextChars, after.RAGKDefault, after.RAGMaxContextChars)
			}
			if got.RAGSystemPrompt != after.RAGSystemPrompt {
				t.Errorf("system_prompt did not load back:\ngot  %q\nwant %q", got.RAGSystemPrompt, after.RAGSystemPrompt)
			}
		})
	}
}

// TestProfileSettingsYAML_NothingToAdd pins that keeping the current settings
// prints nothing.
func TestProfileSettingsYAML_NothingToAdd(t *testing.T) {
	before := config.Default()
	after := before
	setupwizard.ApplyCorpusProfile(&after, setupwizard.ProfileKeep)
	if lines := setupwizard.ProfileSettingsYAML(before, after); lines != nil {
		t.Errorf("lines = %q, want nil", lines)
	}
}
