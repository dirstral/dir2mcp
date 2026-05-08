package tests

import (
	"context"
	"path/filepath"
	"testing"

	"dir2mcp/internal/model"
	"dir2mcp/internal/store"
)

// TestSQLiteStore_SearchBM25_BasicAndBackfill verifies the FTS5-backed BM25
// search method (added in #158) returns hits for matching queries. The same
// test exercises the chunks_fts auto-populate path: the FTS table is filled
// via the AFTER INSERT trigger on chunks, so a SearchBM25 call right after a
// few inserts must already see them.
func TestSQLiteStore_SearchBM25_BasicAndBackfill(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chunks := []struct {
		label uint64
		text  string
	}{
		{1, "the quick brown fox jumps over the lazy dog"},
		{2, "section 35 reporting suspicious transactions financial institution"},
		{3, "discussion of training, qualifications, and reporting officer roles"},
		{4, "drug trafficking penalties under criminal conduct act"},
	}
	for _, c := range chunks {
		task := model.NewChunkTask(c.label, c.text, "text", model.ChunkMetadata{
			ChunkID: c.label,
			RelPath: "docs/case.md",
			DocType: "md",
			RepType: "raw_text",
		})
		if err := st.UpsertChunkTask(ctx, task); err != nil {
			t.Fatalf("UpsertChunkTask(%d): %v", c.label, err)
		}
	}

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}

	hits, err := ls.SearchBM25(ctx, "section 35 suspicious", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one BM25 hit for an exact-term query")
	}
	if hits[0].ChunkID != 2 {
		t.Errorf("expected chunk 2 to be top BM25 hit; got chunk %d (snippet=%q)", hits[0].ChunkID, hits[0].Snippet)
	}

	// Empty query returns no hits, no error.
	hits, err = ls.SearchBM25(ctx, "   ", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25(empty): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty query should return no hits, got %d", len(hits))
	}

	// indexKind filter excludes mismatched chunks.
	hits, err = ls.SearchBM25(ctx, "fox", 5, "code")
	if err != nil {
		t.Fatalf("SearchBM25(code-only): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("index_kind filter should have excluded text chunks, got %d hits", len(hits))
	}
}
