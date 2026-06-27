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
