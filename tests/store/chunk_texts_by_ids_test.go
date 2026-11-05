package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestChunkTextsByIDs_LiveChunksOnly pins the batch text lookup behind
// open_file span reads (PR #1130 review): every live id maps to its stored
// text, and an unknown id, the zero id and a chunk of an errored document are
// absent rather than an error.
func TestChunkTextsByIDs_LiveChunksOnly(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, rel := range []string{"docs/live.md", "docs/broken.md"} {
		status := "ok"
		if rel == "docs/broken.md" {
			status = "error"
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "md", SourceType: "local", Status: status}); err != nil {
			t.Fatalf("UpsertDocument %s: %v", rel, err)
		}
	}
	for id, rel := range map[uint64]string{1: "docs/live.md", 2: "docs/live.md", 3: "docs/broken.md"} {
		if err := st.UpsertChunkTask(ctx, model.NewChunkTask(id, fmt.Sprintf("  text of chunk %d\n", id), "text", model.ChunkMetadata{
			ChunkID: id, RelPath: rel, DocType: "md", RepType: "raw_text",
		})); err != nil {
			t.Fatalf("UpsertChunkTask %d: %v", id, err)
		}
	}

	got, err := st.ChunkTextsByIDs(ctx, []uint64{1, 2, 3, 99, 0})
	if err != nil {
		t.Fatalf("ChunkTextsByIDs: %v", err)
	}
	if len(got) != 2 || got[1] != "  text of chunk 1\n" || got[2] != "  text of chunk 2\n" {
		t.Fatalf("want the two live chunks byte for byte, got %v", got)
	}
	if _, ok := got[3]; ok {
		t.Fatal("a chunk of an errored document must not be served")
	}
	empty, err := st.ChunkTextsByIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ids: got %v, %v", empty, err)
	}
}

// TestChunkTextsByIDs_SpansBatches: more ids than one IN list carries still
// come back complete, so a broad span is never silently cut at the batch size.
func TestChunkTextsByIDs_SpansBatches(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const n = 1203
	ids := make([]uint64, 0, n)
	for i := 1; i <= n; i++ {
		id := uint64(i)
		if err := st.UpsertChunkTask(ctx, model.NewChunkTask(id, fmt.Sprintf("chunk %d", i), "text", model.ChunkMetadata{
			ChunkID: id, RelPath: "docs/long.md", DocType: "md", RepType: "raw_text",
		})); err != nil {
			t.Fatalf("UpsertChunkTask %d: %v", id, err)
		}
		ids = append(ids, id)
	}
	got, err := st.ChunkTextsByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("ChunkTextsByIDs: %v", err)
	}
	if len(got) != n || got[1] != "chunk 1" || got[n] != fmt.Sprintf("chunk %d", n) {
		t.Fatalf("want all %d chunks, got %d", n, len(got))
	}
}
