package tests

import (
	"context"
	"path/filepath"
	"testing"

	"dir2mcp/internal/model"
	"dir2mcp/internal/store"
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

	got, err := st.GetDocumentByPath(ctx, titled.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(titled): %v", err)
	}
	if got.Title != titled.Title {
		t.Errorf("titled doc: title roundtrip failed\n got: %q\nwant: %q", got.Title, titled.Title)
	}

	got, err = st.GetDocumentByPath(ctx, untitled.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(untitled): %v", err)
	}
	if got.Title != "" {
		t.Errorf("untitled doc: expected empty title, got %q", got.Title)
	}

	// ListFiles should also return the title.
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

	// Re-upserting WITHOUT a title must not erase the previously-stored title.
	noTitleUpdate := titled
	noTitleUpdate.Title = ""
	noTitleUpdate.SizeBytes = 9999
	if err := st.UpsertDocument(ctx, noTitleUpdate); err != nil {
		t.Fatalf("UpsertDocument(noTitleUpdate): %v", err)
	}
	got, err = st.GetDocumentByPath(ctx, titled.RelPath)
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
