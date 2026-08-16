package tests

// `ingest.archives.mode` has no runtime effect, and that fact is pinned here on
// observable ingest output: #843.
//
// The shipped default is `deep`, whose name says an archive nested inside an
// archive is expanded. It is not. Nothing in the ingestion runtime reads the
// value, so all three accepted members (`off`, `shallow`, `deep`) index exactly
// the same documents: the top level of one container, and nothing below it.
//
// The assertion is deliberately on the store, not on a flag. A test that read
// cfg.IngestArchivesMode back would pass while the promise stayed broken, which
// is how the gap survived this long. tests/config asserts the operator is told.
//
// This test is the tripwire for the real fix as well. When a dirstral-spec PR
// defines what each member does and dir2mcp implements it, this test MUST fail:
// `off` will stop expanding and `deep` will recurse. Update it then, and only
// with a merged spec change behind it.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// ingestUnderArchivesMode indexes one corpus holding an archive nested inside an
// archive, under the given ingest.archives.mode, and returns the store.
func ingestUnderArchivesMode(t *testing.T, mode string) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	innerZip := buildZip(t, map[string]string{"inner.txt": "nested payload"})
	outerZip := buildZip(t, map[string]string{
		"top.txt":   "top-level payload",
		"inner.zip": string(innerZip),
	})
	if err := os.WriteFile(filepath.Join(root, "outer.zip"), outerZip, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	cfg.IngestArchivesMode = mode
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ingest.archives.mode=%q must validate: %v", mode, err)
	}
	if err := mustNewIngestService(t, cfg, st).Run(ctx); err != nil {
		t.Fatalf("Run under ingest.archives.mode=%q: %v", mode, err)
	}
	return st
}

// sortedDocPaths returns the indexed rel_paths in a stable order.
func sortedDocPaths(t *testing.T, st *store.SQLiteStore) []string {
	t.Helper()
	paths := make([]string, 0, 8)
	for p := range docPaths(t, st) {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// TestArchivesMode_EveryValueIndexesTheSameDocuments is the honesty proof. It
// runs one corpus under each accepted member and compares the resulting document
// sets. They are identical, so no name in the enum describes anything the server
// does differently.
func TestArchivesMode_EveryValueIndexesTheSameDocuments(t *testing.T) {
	want := sortedDocPaths(t, ingestUnderArchivesMode(t, "shallow"))
	if len(want) == 0 {
		t.Fatal("the reference run indexed nothing; the corpus fixture is broken")
	}
	for _, mode := range []string{"off", "deep"} {
		got := sortedDocPaths(t, ingestUnderArchivesMode(t, mode))
		if len(got) != len(want) {
			t.Fatalf("ingest.archives.mode=%q indexed a different document set: got=%v want=%v", mode, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("ingest.archives.mode=%q indexed a different document set: got=%v want=%v", mode, got, want)
			}
		}
	}
}

// TestArchivesMode_OffStillExpandsTheTopLevel pins the direction the name gets
// wrong the other way round. An operator who sets `off` for a privacy or cost
// reason still gets every top-level member of every archive indexed.
func TestArchivesMode_OffStillExpandsTheTopLevel(t *testing.T) {
	paths := docPaths(t, ingestUnderArchivesMode(t, "off"))
	if !paths["outer.zip/top.txt"] {
		t.Errorf("ingest.archives.mode=off does not disable expansion today; want outer.zip/top.txt indexed, got %v", paths)
	}
}

// TestArchivesMode_DeepDoesNotRecurse pins the promise in the issue title. Under
// the shipped default the nested archive is stored as a skipped document, and
// its own member never becomes one, so the container reports the gap instead of
// coverage it does not have (#683, #817).
func TestArchivesMode_DeepDoesNotRecurse(t *testing.T) {
	st := ingestUnderArchivesMode(t, "deep")
	paths := docPaths(t, st)

	if !paths["outer.zip/inner.zip"] {
		t.Fatalf("the nested archive must be recorded as a document; got %v", paths)
	}
	if paths["outer.zip/inner.zip/inner.txt"] {
		t.Errorf("ingest.archives.mode=deep does NOT recurse today; outer.zip/inner.zip/inner.txt must not be indexed")
	}
	nested := documentByPath(t, st, "outer.zip/inner.zip")
	if nested.SkipReason != model.SkipReasonArchive {
		t.Errorf("the unexpanded nested archive must carry skip_reason=%q so the gap is visible; got %q",
			model.SkipReasonArchive, nested.SkipReason)
	}
}
