package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_SearchBM25_ResolvesRealLineSpan pins issue #403 F6: a BM25 hit
// must carry the chunk's persisted line span, not the degenerate `lines 0-0`
// placeholder the query used to hardcode. Before the fix SearchBM25 always set
// Span{Kind:"lines"} with zero lines, and the hybrid path only re-attached a
// real span on a metadata-cache HIT — so a cache miss surfaced a citation
// pointing at non-existent line 0. Resolving the span from the spans table at
// the query boundary makes it correct regardless of cache warmth.
func TestSQLiteStore_SearchBM25_ResolvesRealLineSpan(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const relPath = "docs/lease.md"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h-lease",
		})
		if err != nil {
			return err
		}
		ch := model.Chunk{RepID: repID, Ordinal: 0, Text: "termination notice period is thirty days", IndexKind: "text"}
		_, err = tx.InsertChunkWithSpans(ctx, ch, []model.Span{{Kind: "lines", StartLine: 12, EndLine: 18}})
		return err
	})
	if err != nil {
		t.Fatalf("seed chunk with span: %v", err)
	}

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}
	hits, err := ls.SearchBM25(ctx, "termination notice", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected a BM25 hit for the seeded chunk")
	}
	span := hits[0].Span
	if span.Kind != "lines" || span.StartLine != 12 || span.EndLine != 18 {
		t.Fatalf("BM25 hit span = %+v, want lines 12-18 (regression: degenerate lines 0-0)", span)
	}
}

// TestSQLiteStore_SearchBM25_OmitsSpanWhenNoneStored pins the other half of
// issue #403 F6: a chunk with no persisted span row (e.g. a plain lexical chunk
// written without provenance) must yield an *empty* span, not a misleading
// `lines 0-0`. An empty span is the honest "unknown location" state that
// downstream serializers render as a document-level span; line 0 is a phantom
// citation no client can resolve.
func TestSQLiteStore_SearchBM25_OmitsSpanWhenNoneStored(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// UpsertChunkTask writes the chunk row (and populates chunks_fts) but no
	// span row, so the LEFT JOIN in SearchBM25 finds no provenance.
	task := model.NewChunkTask(1, "quarterly revenue exceeded projections", "text", model.ChunkMetadata{
		ChunkID: 1,
		RelPath: "docs/report.md",
		DocType: "md",
		RepType: "raw_text",
	})
	if err := st.UpsertChunkTask(ctx, task); err != nil {
		t.Fatalf("UpsertChunkTask: %v", err)
	}

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}
	hits, err := ls.SearchBM25(ctx, "quarterly revenue", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected a BM25 hit for the spanless chunk")
	}
	span := hits[0].Span
	if span.Kind != "" {
		t.Fatalf("spanless BM25 hit span = %+v, want empty span (regression: degenerate lines 0-0)", span)
	}
	if span.StartLine != 0 || span.EndLine != 0 {
		t.Fatalf("empty span must carry no line range, got start=%d end=%d", span.StartLine, span.EndLine)
	}
}
