package tests

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// insertChunkWithLanguage upserts a document + representation (carrying the given
// effective language in its meta_json) + one chunk, so the chunk's denormalized
// language column (SPEC §5.2/§8.8) is populated. Returns nothing; fatals on error.
func insertChunkWithLanguage(t *testing.T, st *store.SQLiteStore, relPath, language string) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{RelPath: relPath, DocType: "md"}); err != nil {
		t.Fatalf("upsert document %q: %v", relPath, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("get document %q: %v", relPath, err)
	}
	meta := `{"language":"` + language + `"}`
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:    doc.DocID,
		RepType:  "raw_text",
		RepHash:  relPath + "-h",
		MetaJSON: meta,
	})
	if err != nil {
		t.Fatalf("upsert representation %q: %v", relPath, err)
	}
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:     repID,
		Ordinal:   0,
		Text:      "body of " + relPath,
		IndexKind: "text",
	}, nil); err != nil {
		t.Fatalf("insert chunk %q: %v", relPath, err)
	}
}

// TestListCorpusLanguages_DistinctSorted pins the "auto" cross-lingual target
// resolution backend (#325): the distinct non-empty effective languages recorded
// across non-deleted chunks are returned, sorted and de-duplicated.
func TestListCorpusLanguages_DistinctSorted(t *testing.T) {
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "langs.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	insertChunkWithLanguage(t, st, "a.md", "ru")
	insertChunkWithLanguage(t, st, "b.md", "en")
	insertChunkWithLanguage(t, st, "c.md", "ru") // duplicate language
	insertChunkWithLanguage(t, st, "d.md", "")   // unknown language: excluded

	langs, err := st.ListCorpusLanguages(context.Background())
	if err != nil {
		t.Fatalf("ListCorpusLanguages: %v", err)
	}
	if !reflect.DeepEqual(langs, []string{"en", "ru"}) {
		t.Fatalf("ListCorpusLanguages = %#v, want [en ru]", langs)
	}
}

// TestListCorpusLanguages_EmptyCorpus pins that an empty corpus yields no
// languages (so "auto" cross-lingual expansion is a no-op).
func TestListCorpusLanguages_EmptyCorpus(t *testing.T) {
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	langs, err := st.ListCorpusLanguages(context.Background())
	if err != nil {
		t.Fatalf("ListCorpusLanguages: %v", err)
	}
	if len(langs) != 0 {
		t.Fatalf("expected no languages for empty corpus, got %#v", langs)
	}
}
