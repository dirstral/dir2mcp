package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestListFiles_ExcludesDeletedDocuments verifies that soft-deleted documents
// are not returned by ListFiles.
func TestListFiles_ExcludesDeletedDocuments(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	// insert a live document and a deleted document
	live := model.Document{RelPath: "live.txt", DocType: "text", Status: "ok"}
	gone := model.Document{RelPath: "gone.txt", DocType: "text", Status: "ok"}

	if err := st.UpsertDocument(ctx, live); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	if err := st.UpsertDocument(ctx, gone); err != nil {
		t.Fatalf("upsert gone: %v", err)
	}
	if err := st.MarkDocumentDeleted(ctx, "gone.txt"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	docs, total, err := st.ListFiles(ctx, "", "", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	for _, d := range docs {
		if d.RelPath == "gone.txt" {
			t.Errorf("ListFiles returned deleted document %q", d.RelPath)
		}
	}

	if total != 1 {
		t.Errorf("ListFiles total=%d want 1", total)
	}
	if len(docs) != 1 || docs[0].RelPath != "live.txt" {
		t.Errorf("ListFiles returned unexpected docs: %v", docs)
	}
}

// TestListFiles_TotalReflectsOnlyLiveDocs verifies that the total count
// returned by ListFiles excludes deleted documents.
func TestListFiles_TotalReflectsOnlyLiveDocs(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("dir/file%c.txt", 'a'+i)
		doc := model.Document{RelPath: path, DocType: "text", Status: "ok"}
		if err := st.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	// delete two of them
	for _, p := range []string{"dir/filea.txt", "dir/fileb.txt"} {
		if err := st.MarkDocumentDeleted(ctx, p); err != nil {
			t.Fatalf("mark deleted %s: %v", p, err)
		}
	}

	_, total, err := st.ListFiles(ctx, "", "", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if total != 3 {
		t.Errorf("ListFiles total=%d want 3", total)
	}
}
