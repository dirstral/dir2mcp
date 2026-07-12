package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestChunkMetadata_MTimeDenormalizedFromDocument pins the SPEC §9.6 denorm: the
// source document's calendar anchor (mtime_unix) is surfaced onto the chunk
// metadata every metadata-loading query returns — NextPending (live embed),
// ChunkTaskByID (distributed embed), and ListEmbeddedChunkMetadata (warm reload)
// — so the date/time-range retrieval filter can predicate on candidates without
// a per-hit store lookup, mirroring the per-language (§9.5) denorm.
func TestChunkMetadata_MTimeDenormalizedFromDocument(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const relPath = "docs/dated.md"
	wantMTime := time.Date(2026, 4, 15, 9, 30, 0, 0, time.UTC).Unix()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "local", Status: "ok", MTimeUnix: wantMTime,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	pendingID, embeddedID := seedDatedRepChunks(t, st, doc.DocID)

	// NextPending (live embed path → OnIndexedChunk → chunkByLabel).
	pending, err := st.NextPending(ctx, 100, "")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected at least one pending chunk")
	}
	assertTasksMTime(t, "NextPending", pending, wantMTime)

	// ChunkTaskByID (distributed embed lease path).
	task, _, err := st.ChunkTaskByID(ctx, uint64(pendingID))
	if err != nil {
		t.Fatalf("ChunkTaskByID: %v", err)
	}
	assertTasksMTime(t, "ChunkTaskByID", []model.ChunkTask{task}, wantMTime)

	// ListEmbeddedChunkMetadata (warm-reload path → chunkByLabel after restart).
	embedded, err := st.ListEmbeddedChunkMetadata(ctx, "text", 100, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata: %v", err)
	}
	assertTasksMTime(t, "ListEmbeddedChunkMetadata", embedded, wantMTime)
	if !containsChunk(embedded, uint64(embeddedID)) {
		t.Fatalf("embedded chunk %d not returned by ListEmbeddedChunkMetadata", embeddedID)
	}
}

// seedDatedRepChunks inserts one pending and one embedded ("ok") chunk under a new
// representation of docID, returning their chunk ids.
func seedDatedRepChunks(t *testing.T, st *store.SQLiteStore, docID int64) (pendingID, embeddedID int64) {
	t.Helper()
	ctx := context.Background()
	err := st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: docID, RepType: "raw_text", RepHash: "h1",
		})
		if rerr != nil {
			return rerr
		}
		id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "pending chunk", TextHash: "p1", IndexKind: "text",
			EmbeddingStatus: "pending",
		}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}})
		if cerr != nil {
			return cerr
		}
		pendingID = id
		embeddedID, cerr = tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 1, Text: "embedded chunk", TextHash: "e1", IndexKind: "text",
			EmbeddingStatus: "ok",
		}, []model.Span{{Kind: "lines", StartLine: 2, EndLine: 2}})
		return cerr
	})
	if err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	return pendingID, embeddedID
}

// assertTasksMTime fails unless every task carries the expected document mtime.
func assertTasksMTime(t *testing.T, from string, tasks []model.ChunkTask, want int64) {
	t.Helper()
	for _, tk := range tasks {
		if tk.Metadata.MTimeUnix != want {
			t.Fatalf("%s chunk %d mtime = %d, want %d", from, tk.Metadata.ChunkID, tk.Metadata.MTimeUnix, want)
		}
	}
}

// containsChunk reports whether tasks include a chunk with the given id.
func containsChunk(tasks []model.ChunkTask, id uint64) bool {
	for _, tk := range tasks {
		if tk.Metadata.ChunkID == id {
			return true
		}
	}
	return false
}
