package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_DocumentTitleRoundtrip verifies that the title column
// (added for #159) persists across upsert and is returned by both
// GetDocumentByPath and ListFiles. Empty titles must remain empty.
func TestSQLiteStore_DocumentTitleRoundtrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	titled := model.Document{
		RelPath:   "acts/proliferation.pdf",
		DocType:   "pdf",
		Title:     "Proliferation Financing (Prohibition) Act, 2021",
		SizeBytes: 1234,
		MTimeUnix: 1_700_000_000,
		Status:    "ok",
	}
	untitled := model.Document{
		RelPath:   "notes/raw.md",
		DocType:   "md",
		SizeBytes: 100,
		MTimeUnix: 1_700_000_001,
		Status:    "ok",
	}
	for _, d := range []model.Document{titled, untitled} {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.RelPath, err)
		}
	}

	assertGetTitleByPath(t, ctx, st, titled.RelPath, titled.Title)
	assertGetTitleByPath(t, ctx, st, untitled.RelPath, "")
	assertListFilesTitles(t, ctx, st, titled, untitled)
	assertUpsertEmptyTitlePreserves(t, ctx, st, titled)
}

func assertGetTitleByPath(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath, wantTitle string) {
	t.Helper()
	got, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if got.Title != wantTitle {
		t.Errorf("doc %s: title mismatch\n got: %q\nwant: %q", relPath, got.Title, wantTitle)
	}
}

func assertListFilesTitles(t *testing.T, ctx context.Context, st *store.SQLiteStore, titled, untitled model.Document) {
	t.Helper()
	docs, _, err := st.ListFiles(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, d := range docs {
		switch d.RelPath {
		case titled.RelPath:
			if d.Title != titled.Title {
				t.Errorf("ListFiles: titled doc title mismatch: got %q want %q", d.Title, titled.Title)
			}
		case untitled.RelPath:
			if d.Title != "" {
				t.Errorf("ListFiles: untitled doc should have empty title, got %q", d.Title)
			}
		}
	}
}

func assertUpsertEmptyTitlePreserves(t *testing.T, ctx context.Context, st *store.SQLiteStore, titled model.Document) {
	t.Helper()
	noTitleUpdate := titled
	noTitleUpdate.Title = ""
	noTitleUpdate.SizeBytes = 9999
	if err := st.UpsertDocument(ctx, noTitleUpdate); err != nil {
		t.Fatalf("UpsertDocument(noTitleUpdate): %v", err)
	}
	got, err := st.GetDocumentByPath(ctx, titled.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath after empty-title update: %v", err)
	}
	if got.Title != titled.Title {
		t.Errorf("title was erased by empty-title upsert: got %q want %q", got.Title, titled.Title)
	}
	if got.SizeBytes != 9999 {
		t.Errorf("size_bytes update did not propagate: got %d", got.SizeBytes)
	}
}
