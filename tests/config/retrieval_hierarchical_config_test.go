package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// The retrieval.hierarchical config block (SPEC §9.7/§16.2, dir2mcp #329).

// TestRetrievalHierarchical_DefaultDisabled pins the local-first default:
// hierarchical retrieval is OFF, `source_reps` is the `auto` sentinel (nil),
// only document-level summaries are configured, and the generation bound and
// prompt version carry their spec defaults.
func TestRetrievalHierarchical_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalHierarchicalEnabled {
		t.Fatal("retrieval.hierarchical.enabled must default to false")
	}
	if len(cfg.RetrievalHierarchicalSourceReps) != 0 {
		t.Fatalf("source_reps must default to auto (empty), got %v", cfg.RetrievalHierarchicalSourceReps)
	}
	if len(cfg.RetrievalHierarchicalLevels) != 1 || cfg.RetrievalHierarchicalLevels[0] != config.HierarchicalLevelDocument {
		t.Fatalf("levels must default to [document], got %v", cfg.RetrievalHierarchicalLevels)
	}
	if cfg.RetrievalHierarchicalMaxTokens != config.DefaultHierarchicalMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", cfg.RetrievalHierarchicalMaxTokens, config.DefaultHierarchicalMaxTokens)
	}
	if cfg.RetrievalHierarchicalPromptVersion != config.DefaultHierarchicalPromptVersion {
		t.Fatalf("prompt_version = %q, want %q", cfg.RetrievalHierarchicalPromptVersion, config.DefaultHierarchicalPromptVersion)
	}
	if cfg.HierarchicalDocumentLevelEnabled() {
		t.Fatal("document-level summaries must not be active while the feature is disabled")
	}
}

// TestRetrievalHierarchical_NestedYAMLRoundTrips pins the nested block exactly
// as the spec documents it, including the scalar `source_reps: auto` sentinel.
func TestRetrievalHierarchical_NestedYAMLRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  hierarchical:",
		"    enabled: true",
		"    source_reps: auto",
		"    levels: [document]",
		"    provider: openai",
		"    max_tokens: 256",
		"    prompt_version: v1",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalHierarchicalEnabled {
		t.Fatal("nested retrieval.hierarchical.enabled = false, want true")
	}
	if len(cfg.RetrievalHierarchicalSourceReps) != 0 {
		t.Fatalf("the `auto` sentinel must normalize to an empty list, got %v", cfg.RetrievalHierarchicalSourceReps)
	}
	if cfg.RetrievalHierarchicalProvider != "openai" {
		t.Fatalf("provider = %q, want openai", cfg.RetrievalHierarchicalProvider)
	}
	if cfg.RetrievalHierarchicalMaxTokens != 256 {
		t.Fatalf("max_tokens = %d, want 256", cfg.RetrievalHierarchicalMaxTokens)
	}
	if !cfg.HierarchicalDocumentLevelEnabled() {
		t.Fatal("document-level summaries must be active for levels: [document]")
	}
	if cfg.HierarchicalSectionLevelRequested() {
		t.Fatal("section level must not be requested for levels: [document]")
	}
}

// TestRetrievalHierarchical_ExplicitSourceRepsList pins the block-sequence form
// of `source_reps`, which selects a distinct summary per named representation.
func TestRetrievalHierarchical_ExplicitSourceRepsList(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  hierarchical:",
		"    enabled: true",
		"    source_reps:",
		"      - extracted_markdown",
		"      - transcript",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{"extracted_markdown", "transcript"}
	got := cfg.RetrievalHierarchicalSourceReps
	if len(got) != len(want) {
		t.Fatalf("source_reps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source_reps = %v, want %v (declared order preserved)", got, want)
		}
	}
}

// TestRetrievalHierarchical_FlatAliasRoundTrips pins the flat snake_case aliases
// used by the effective-config snapshot.
func TestRetrievalHierarchical_FlatAliasRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_hierarchical_enabled: true",
		"retrieval_hierarchical_max_tokens: 128",
		"retrieval_hierarchical_prompt_version: v2",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalHierarchicalEnabled {
		t.Fatal("flat retrieval_hierarchical_enabled = false, want true")
	}
	if cfg.RetrievalHierarchicalMaxTokens != 128 {
		t.Fatalf("flat max_tokens = %d, want 128", cfg.RetrievalHierarchicalMaxTokens)
	}
	if cfg.RetrievalHierarchicalPromptVersion != "v2" {
		t.Fatalf("flat prompt_version = %q, want v2", cfg.RetrievalHierarchicalPromptVersion)
	}
}

// TestRetrievalHierarchical_SectionLevelIsAcceptedAndFlagged pins forward
// compatibility with the section follow-up: `section` is a valid level, is
// reported as requested (so the ingest layer can warn honestly), and does not
// disable document-level summaries when both are configured.
func TestRetrievalHierarchical_SectionLevelIsAcceptedAndFlagged(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  hierarchical:",
		"    enabled: true",
		"    levels: [document, section]",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.HierarchicalDocumentLevelEnabled() {
		t.Fatal("document-level summaries must remain active alongside section")
	}
	if !cfg.HierarchicalSectionLevelRequested() {
		t.Fatal("section level must be reported as requested so it can be warned about")
	}
}

// TestRetrievalHierarchical_InvalidValuesAreConfigInvalid pins that a typo fails
// deterministically at config time rather than lying dormant until the feature
// is switched on: an unknown level and a negative token bound are both rejected,
// even with the feature disabled.
func TestRetrievalHierarchical_InvalidValuesAreConfigInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml []string
		want string
	}{
		{
			name: "unknown level",
			yaml: []string{"retrieval:", "  hierarchical:", "    levels: [chapter]"},
			want: "retrieval.hierarchical.levels",
		},
		{
			name: "negative max_tokens",
			yaml: []string{"retrieval_hierarchical_max_tokens: -1"},
			want: "retrieval.hierarchical.max_tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, strings.Join(append(tc.yaml, ""), "\n"))
			_, err := config.LoadFile(path)
			if err == nil {
				t.Fatalf("expected a CONFIG_INVALID error mentioning %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}
