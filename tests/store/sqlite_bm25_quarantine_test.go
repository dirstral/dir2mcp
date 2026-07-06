package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_SearchBM25_ExcludesQuarantined is a regression guard for #439
// (F1): chunks the quality gate quarantined (embedding_status='error') must not
// be returned by the lexical/hybrid path, even though the FTS index still
// contains their terms. An 'ok' chunk matching the same term must still surface.
func TestSQLiteStore_SearchBM25_ExcludesQuarantined(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const (
		okLabel         uint64 = 1
		quarantineLabel uint64 = 2
	)
	chunks := []struct {
		label uint64
		text  string
	}{
		{okLabel, "reporting suspicious transactions to the regulator"},
		{quarantineLabel, "reporting officer training and qualifications"},
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

	// Mark one chunk embedded ('ok') and quarantine the other ('error').
	if err := st.MarkEmbedded(ctx, []uint64{okLabel}); err != nil {
		t.Fatalf("MarkEmbedded: %v", err)
	}
	if err := st.MarkFailed(ctx, []uint64{quarantineLabel}, "quality gate quarantine"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}

	hits, err := ls.SearchBM25(ctx, "reporting", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly the 'ok' chunk to match, got %d hits: %+v", len(hits), hits)
	}
	if hits[0].ChunkID != okLabel {
		t.Errorf("expected ok chunk %d, got chunk %d", okLabel, hits[0].ChunkID)
	}
	for _, h := range hits {
		if h.ChunkID == quarantineLabel {
			t.Errorf("quarantined chunk %d (embedding_status='error') must not appear in BM25 results", quarantineLabel)
		}
	}
}

// TestSQLiteStore_SearchBM25_ExcludesQuarantinedAfterEmbedding is the lifecycle
// regression for #439 (F1): the realistic quarantine path is a chunk that was
// embedded (embedding_status='ok', already live and searchable, its FTS row
// present) and is *later* re-screened and quarantined via MarkFailed. Because
// the chunks_ai/au triggers keep every chunk's terms in chunks_fts regardless
// of status — and MUST, so the #405/#506 drift check
// (COUNT(chunks_fts_docsize) == COUNT(chunks)) stays valid — the row survives
// the ok->error transition. It is the query-time WHERE filter, not the trigger,
// that has to exclude it. This test proves that: both chunks surface while
// healthy, then only the still-'ok' chunk surfaces once its sibling flips to
// 'error'. (The sibling test above covers a chunk quarantined before it was
// ever embedded.)
func TestSQLiteStore_SearchBM25_ExcludesQuarantinedAfterEmbedding(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const (
		healthyLabel    uint64 = 1
		quarantineLabel uint64 = 2
	)
	chunks := []struct {
		label uint64
		text  string
	}{
		{healthyLabel, "reporting suspicious transactions to the regulator"},
		{quarantineLabel, "reporting officer training and qualifications"},
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

	// Both chunks are embedded and healthy: both are live and must match.
	if err := st.MarkEmbedded(ctx, []uint64{healthyLabel, quarantineLabel}); err != nil {
		t.Fatalf("MarkEmbedded: %v", err)
	}
	before, err := ls.SearchBM25(ctx, "reporting", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25 (pre-quarantine): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected both 'ok' chunks to match before quarantine, got %d hits: %+v", len(before), before)
	}

	// Re-screen quarantines one chunk: ok -> error. Its FTS row is untouched by
	// the status update, so only the WHERE filter can keep it out of results.
	if err := st.MarkFailed(ctx, []uint64{quarantineLabel}, "quality gate quarantine"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	after, err := ls.SearchBM25(ctx, "reporting", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25 (post-quarantine): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected only the still-'ok' chunk after quarantine, got %d hits: %+v", len(after), after)
	}
	if after[0].ChunkID != healthyLabel {
		t.Errorf("expected healthy chunk %d, got chunk %d", healthyLabel, after[0].ChunkID)
	}
	if after[0].ChunkID == quarantineLabel {
		t.Errorf("chunk %d quarantined after embedding (embedding_status='error') must not appear in BM25 results", quarantineLabel)
	}
}
