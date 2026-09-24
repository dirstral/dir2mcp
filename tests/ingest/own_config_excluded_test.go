package tests

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §7.2 (0.73.0): the default security.path_excludes lists the server's own
// configuration file and both dotenv files it reads. A first run on a fresh
// folder used to index .dir2mcp.yaml and cite it as a source for an unrelated
// question.
func TestOwnConfigFilesAreNotIndexedByDefault(t *testing.T) {
	for _, want := range []string{"**/.dir2mcp.yaml", "**/.env", "**/.env.local"} {
		if !slices.Contains(config.Default().PathExcludes, want) {
			t.Errorf("default security.path_excludes lacks %q", want)
		}
	}

	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "The budget meeting moved to Thursday.\n")
	writeFile(t, filepath.Join(root, ".dir2mcp.yaml"), "providers:\n  local:\n    kind: openai\n    api_key: sk-not-a-real-key\n")
	writeFile(t, filepath.Join(root, ".env.local"), "MISTRAL_API_KEY=not-a-real-key\n")
	writeFile(t, filepath.Join(root, "sub", ".dir2mcp.yaml"), "ingest:\n  extractor: off\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	defer func() { _ = st.Close() }()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := st.GetDocumentByPath(ctx, "notes.md"); err != nil {
		t.Fatalf("notes.md must be indexed: %v", err)
	}
	for _, rel := range []string{".dir2mcp.yaml", ".env.local", "sub/.dir2mcp.yaml"} {
		if doc, err := st.GetDocumentByPath(ctx, rel); err == nil && doc.Status != "skipped" {
			t.Errorf("%s was indexed (status %q); the server's own config must be excluded", rel, doc.Status)
		}
	}
}
