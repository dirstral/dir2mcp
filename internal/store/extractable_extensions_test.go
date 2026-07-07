package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestExtractableExtensionCounts drives the store query that feeds the doctor
// extraction-coverage diagnostic (#395): it must count only non-deleted,
// index-eligible (status='ok') documents in the extractable pdf/image/document
// buckets, keyed by lowercased extension, and ignore code/text/deleted/errored
// rows.
func TestExtractableExtensionCounts(t *testing.T) {
	st := NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}

	docs := []model.Document{
		{RelPath: "a.PDF", DocType: "pdf", Status: "ok", ContentHash: "h1"},   // uppercase ext → normalized
		{RelPath: "b.pdf", DocType: "pdf", Status: "ok", ContentHash: "h2"},   // second .pdf
		{RelPath: "scan.tiff", DocType: "image", Status: "ok", ContentHash: "h3"},
		{RelPath: "notes.odt", DocType: "document", Status: "ok", ContentHash: "h4"},
		{RelPath: "main.go", DocType: "code", Status: "ok", ContentHash: "h5"},        // not extractable
		{RelPath: "bad.docx", DocType: "document", Status: "error", ContentHash: "h6"}, // wrong status
		{RelPath: "gone.pdf", DocType: "pdf", Status: "ok", Deleted: true, ContentHash: "h7"}, // deleted
	}
	for _, d := range docs {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("upsert %s: %v", d.RelPath, err)
		}
	}

	got, err := st.ExtractableExtensionCounts(ctx, "ok")
	if err != nil {
		t.Fatalf("ExtractableExtensionCounts: %v", err)
	}
	want := map[string]int64{
		".pdf":  2,
		".tiff": 1,
		".odt":  1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractableExtensionCounts = %v, want %v", got, want)
	}
}
