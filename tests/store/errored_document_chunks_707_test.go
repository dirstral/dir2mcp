package tests

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// newErroredDocStore opens a fresh store for the #707 suite.
func newErroredDocStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// setDocStatus upserts one document row with the given lifecycle status.
func setDocStatus(t *testing.T, st *store.SQLiteStore, relPath, status string) {
	t.Helper()
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: relPath,
		DocType: "md",
		Status:  status,
	}); err != nil {
		t.Fatalf("UpsertDocument(%s, %s): %v", relPath, status, err)
	}
}

// addEmbeddedChunk inserts one chunk for a document and marks it embedded, so
// it is live by every chunk-level rule: embedding_status='ok' and deleted=0.
func addEmbeddedChunk(t *testing.T, st *store.SQLiteStore, id uint64, relPath, text string) {
	t.Helper()
	ctx := context.Background()
	task := model.NewChunkTask(id, text, "text", model.ChunkMetadata{
		ChunkID: id,
		RelPath: relPath,
		DocType: "md",
		RepType: "raw_text",
	})
	if err := st.UpsertChunkTask(ctx, task); err != nil {
		t.Fatalf("UpsertChunkTask(%d): %v", id, err)
	}
	if err := st.MarkEmbedded(ctx, []uint64{id}); err != nil {
		t.Fatalf("MarkEmbedded(%d): %v", id, err)
	}
}

// TestSearchBM25_ExcludesChunksOfErroredDocument is the lexical half of #707.
//
// A document indexes and embeds on one run. A later scan fails on it, so
// persistBuildError writes status='error'. The chunks of the earlier good run
// keep embedding_status='ok' and deleted=0, and their terms stay in the FTS
// index, so only a query-time parent filter can hide them. Status and doctor
// already report the document as failed, so a lexical hit on it would let the
// corpus cite content that the operator believes is out of service.
//
// The test also pins the recovery direction: the chunks come back the moment
// the document indexes again. That is what makes the query-time filter better
// than a delete on failure, because a transient failure costs no re-embedding.
func TestSearchBM25_ExcludesChunksOfErroredDocument(t *testing.T) {
	ctx := context.Background()
	st := newErroredDocStore(t)

	const (
		erroredChunk uint64 = 1
		healthyChunk uint64 = 2
	)
	setDocStatus(t, st, "docs/broken.md", "ok")
	setDocStatus(t, st, "docs/healthy.md", "ok")
	addEmbeddedChunk(t, st, erroredChunk, "docs/broken.md", "quarterly revenue reconciliation notes")
	addEmbeddedChunk(t, st, healthyChunk, "docs/healthy.md", "quarterly planning notes")

	hits, err := st.SearchBM25(ctx, "quarterly", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25 (both documents ok): %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits while both documents are ok, want 2: %+v", len(hits), hits)
	}

	// The later scan fails on the first document.
	setDocStatus(t, st, "docs/broken.md", "error")

	hits, err = st.SearchBM25(ctx, "quarterly", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25 (one document errored): %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != healthyChunk {
		t.Fatalf("got %+v, want only chunk %d; a document in the error state must not be searchable", hits, healthyChunk)
	}

	// The document indexes again, so its chunks are live again.
	setDocStatus(t, st, "docs/broken.md", "ok")

	hits, err = st.SearchBM25(ctx, "quarterly", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25 (document recovered): %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits after the document indexed again, want 2: %+v", len(hits), hits)
	}
}

// TestSearchBM25_ExcludesChunksOfDeletedDocument covers the other half of the
// #707 predicate. MarkDocumentDeleted tombstones the chunks too, so this guards
// a document row that carries deleted=1 while a chunk row was missed.
func TestSearchBM25_ExcludesChunksOfDeletedDocument(t *testing.T) {
	ctx := context.Background()
	st := newErroredDocStore(t)

	setDocStatus(t, st, "docs/gone.md", "ok")
	addEmbeddedChunk(t, st, 1, "docs/gone.md", "withdrawn advisory text")
	if err := st.MarkDocumentDeleted(ctx, "docs/gone.md"); err != nil {
		t.Fatalf("MarkDocumentDeleted: %v", err)
	}
	// Re-mark the chunk live to isolate the parent rule from the chunk rule.
	if err := st.UpsertChunkTask(ctx, model.NewChunkTask(1, "withdrawn advisory text", "text", model.ChunkMetadata{
		ChunkID: 1,
		RelPath: "docs/gone.md",
		DocType: "md",
		RepType: "raw_text",
	})); err != nil {
		t.Fatalf("UpsertChunkTask (revive chunk): %v", err)
	}
	if err := st.MarkEmbedded(ctx, []uint64{1}); err != nil {
		t.Fatalf("MarkEmbedded: %v", err)
	}

	hits, err := st.SearchBM25(ctx, "advisory", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %+v, want no hits; a chunk of a deleted document must not be searchable", hits)
	}
}

// TestChunkTaskByID_ErroredDocument is the vector half of #707. Retrieval calls
// ChunkTaskByID to test whether a vector hit is still live, so this lookup
// decides whether a failed document's earlier vectors can be returned, reranked
// and put in the answer context. It must report model.ErrNotFound, which is the
// signal the liveness pass acts on.
func TestChunkTaskByID_ErroredDocument(t *testing.T) {
	ctx := context.Background()
	st := newErroredDocStore(t)

	setDocStatus(t, st, "docs/broken.md", "ok")
	addEmbeddedChunk(t, st, 7, "docs/broken.md", "superseded payroll figures")

	if _, _, err := st.ChunkTaskByID(ctx, 7); err != nil {
		t.Fatalf("ChunkTaskByID while the document is ok: %v", err)
	}

	setDocStatus(t, st, "docs/broken.md", "error")

	_, _, err := st.ChunkTaskByID(ctx, 7)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ChunkTaskByID = %v, want model.ErrNotFound; a chunk of a failed document is not live", err)
	}

	setDocStatus(t, st, "docs/broken.md", "ok")
	if _, _, err := st.ChunkTaskByID(ctx, 7); err != nil {
		t.Fatalf("ChunkTaskByID after the document indexed again: %v", err)
	}
}

// TestEmbeddedChunksByPath_ErroredDocument is the related-document half of
// #707. dir2mcp_related seeds a query-by-example from these chunks, so a failed
// document could otherwise still pull neighbours out of the corpus on the
// strength of content the corpus reports as failed. An empty result maps to the
// tool's "source could not be located" answer, which is the honest reply.
func TestEmbeddedChunksByPath_ErroredDocument(t *testing.T) {
	ctx := context.Background()
	st := newErroredDocStore(t)

	setDocStatus(t, st, "docs/broken.md", "ok")
	addEmbeddedChunk(t, st, 11, "docs/broken.md", "alpha")
	addEmbeddedChunk(t, st, 12, "docs/broken.md", "beta")

	got, err := st.EmbeddedChunksByPath(ctx, "docs/broken.md")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath while the document is ok: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks while the document is ok, want 2", len(got))
	}

	setDocStatus(t, st, "docs/broken.md", "error")

	got, err = st.EmbeddedChunksByPath(ctx, "docs/broken.md")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath after the document failed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no chunks; a failed document must not seed a related-document query", got)
	}

	setDocStatus(t, st, "docs/broken.md", "ok")
	got, err = st.EmbeddedChunksByPath(ctx, "docs/broken.md")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath after the document indexed again: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks after the document indexed again, want 2", len(got))
	}
}

// TestLiveChunkRules_TolerateMissingDocumentRow pins the LEFT JOIN half of the
// #707 predicate. A chunk with no document row stays live on every path. Tests
// and older data carry such chunks, and a missing parent is not evidence of a
// failure, so the rule must not hide them.
func TestLiveChunkRules_TolerateMissingDocumentRow(t *testing.T) {
	ctx := context.Background()
	st := newErroredDocStore(t)

	addEmbeddedChunk(t, st, 21, "docs/orphan.md", "orphan chunk text")

	hits, err := st.SearchBM25(ctx, "orphan", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 21 {
		t.Fatalf("SearchBM25 = %+v, want chunk 21; a chunk with no document row stays live", hits)
	}
	if _, _, err := st.ChunkTaskByID(ctx, 21); err != nil {
		t.Fatalf("ChunkTaskByID: %v", err)
	}
	got, err := st.EmbeddedChunksByPath(ctx, "docs/orphan.md")
	if err != nil {
		t.Fatalf("EmbeddedChunksByPath: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("EmbeddedChunksByPath = %d chunks, want 1", len(got))
	}
}
