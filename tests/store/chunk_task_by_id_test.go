package tests

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestChunkTaskByID_RoundTrip pins that ChunkTaskByID (added for distributed
// embed-workers, SPEC §8.7.4) returns the full live task for a chunk id, with its
// text and text_hash, so a worker can embed a leased job without coordinator-
// relayed bytes.
func TestChunkTaskByID_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := st.UpsertChunkTask(ctx, model.NewChunkTask(55, "the chunk text", "text", model.ChunkMetadata{
		ChunkID: 55, RelPath: "docs/a.md", DocType: "md", RepType: "raw_text",
	})); err != nil {
		t.Fatalf("UpsertChunkTask: %v", err)
	}

	task, _, err := st.ChunkTaskByID(ctx, 55)
	if err != nil {
		t.Fatalf("ChunkTaskByID: %v", err)
	}
	if task.Metadata.ChunkID != 55 || task.Text != "the chunk text" || task.IndexKind != "text" {
		t.Fatalf("unexpected task: %+v", task)
	}
}

// TestChunkTaskByID_MissingIsNotFound pins that an unknown chunk id is reported
// as model.ErrNotFound (a worker treats it as a safe skip).
func TestChunkTaskByID_MissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, _, err := st.ChunkTaskByID(ctx, 999); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing chunk err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.ChunkTaskByID(ctx, 0); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("zero chunk err = %v, want ErrNotFound", err)
	}
}

// TestChunkTaskByID_TombstonedIsNotFound pins tombstone safety (SPEC §8.7.3 /
// §6.6): a chunk whose document was tombstoned (deleted=1) is reported as
// ErrNotFound, so a leased job cannot resurrect a deleted chunk.
func TestChunkTaskByID_TombstonedIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const relPath = "docs/deleteme.md"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	var chunkID int64
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h1",
		})
		if rerr != nil {
			return rerr
		}
		id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "doomed", TextHash: "th", IndexKind: "text",
			EmbeddingStatus: "pending",
		}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}})
		chunkID = id
		return cerr
	})
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	// Live before deletion, with its text_hash surfaced.
	_, hash, err := st.ChunkTaskByID(ctx, uint64(chunkID))
	if err != nil {
		t.Fatalf("pre-delete ChunkTaskByID: %v", err)
	}
	if hash != "th" {
		t.Fatalf("text_hash = %q, want th", hash)
	}

	if err := st.MarkDocumentDeleted(ctx, relPath); err != nil {
		t.Fatalf("MarkDocumentDeleted: %v", err)
	}

	if _, _, err := st.ChunkTaskByID(ctx, uint64(chunkID)); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("tombstoned chunk err = %v, want ErrNotFound", err)
	}
}
