package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// insertRelatedChunk inserts one chunk (pending) for the given document.
func insertRelatedChunk(t *testing.T, st *SQLiteStore, id uint64, relPath, text, indexKind string) {
	t.Helper()
	task := model.NewChunkTask(id, text, indexKind, model.ChunkMetadata{
		ChunkID: id,
		RelPath: relPath,
		DocType: "txt",
		RepType: "raw_text",
	})
	if err := st.UpsertChunkTask(context.Background(), task); err != nil {
		t.Fatalf("UpsertChunkTask(%d): %v", id, err)
	}
}

// TestEmbeddedChunksByPath returns only embedded, non-deleted chunks of one
// document, ordered by chunk_id, carrying text + index_kind — the seed material
// dir2mcp_related's rel_path path aggregates (SPEC §15.12).
func TestEmbeddedChunksByPath(t *testing.T) {
	ctx := context.Background()
	st := NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = st.Close() }()

	insertRelatedChunk(t, st, 1, "docs/a.txt", "alpha", "text")
	insertRelatedChunk(t, st, 2, "docs/a.txt", "alpha-two", "code")
	insertRelatedChunk(t, st, 3, "docs/a.txt", "pending-not-embedded", "text")
	insertRelatedChunk(t, st, 4, "docs/b.txt", "beta", "text")

	// Only 1, 2 (of doc a) and 4 are embedded; chunk 3 stays pending.
	if err := st.MarkEmbedded(ctx, []uint64{1, 2, 4}); err != nil {
		t.Fatalf("MarkEmbedded: %v", err)
	}

	got, err := st.EmbeddedChunksByPath(ctx, "docs/a.txt")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath(docs/a.txt): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2 (embedded only, pending excluded): %+v", len(got), got)
	}
	if got[0].Label != 1 || got[1].Label != 2 {
		t.Fatalf("chunk order = [%d %d], want [1 2] (ascending chunk_id)", got[0].Label, got[1].Label)
	}
	if got[0].Text != "alpha" || got[0].IndexKind != "text" {
		t.Fatalf("chunk 1 = (%q,%q), want (alpha,text)", got[0].Text, got[0].IndexKind)
	}
	if got[1].IndexKind != "code" {
		t.Fatalf("chunk 2 index_kind = %q, want code", got[1].IndexKind)
	}

	other, err := st.EmbeddedChunksByPath(ctx, "docs/b.txt")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath(docs/b.txt): %v", err)
	}
	if len(other) != 1 || other[0].Label != 4 {
		t.Fatalf("docs/b.txt = %+v, want single chunk 4", other)
	}

	empty, err := st.EmbeddedChunksByPath(ctx, "docs/does-not-exist.txt")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath(missing): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("missing document = %+v, want empty (resolves to no indexed chunk)", empty)
	}
}
