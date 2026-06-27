package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedDocWithPendingChunk creates a document at relPath with the given status and
// a single live, pending text chunk hung off a fresh representation. It returns
// the chunk id so callers can assert NextPending visibility.
func seedDocWithPendingChunk(ctx context.Context, t *testing.T, st *store.SQLiteStore, relPath, status string) int64 {
	t.Helper()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "filesystem", Status: status,
	}); err != nil {
		t.Fatalf("UpsertDocument(%s, %s): %v", relPath, status, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}

	var chunkID int64
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h-" + relPath,
		})
		if rerr != nil {
			return rerr
		}
		id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "body of " + relPath, TextHash: "th-" + relPath,
			IndexKind: "text", EmbeddingStatus: "pending",
		}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}})
		chunkID = id
		return cerr
	})
	if err != nil {
		t.Fatalf("seed chunk for %s: %v", relPath, err)
	}
	return chunkID
}

// TestDocumentStatus_SecretExcludedRoundTrips pins issue #425 invariant 1: a
// document withheld because it contains secrets is persisted faithfully as
// "secret_excluded" (not collapsed to "ok" by normalizeStatus) and is counted as
// skipped, not indexed, so the audit signal survives.
func TestDocumentStatus_SecretExcludedRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const relPath = "secrets/leaked.env"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "env", SourceType: "filesystem", Status: "secret_excluded",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Status != "secret_excluded" {
		t.Fatalf("status = %q, want secret_excluded (must not collapse to ok)", doc.Status)
	}

	// Add an ordinary indexed doc so we can tell indexed/skipped apart.
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "docs/ok.md", DocType: "md", SourceType: "filesystem", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument(ok): %v", err)
	}

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.Indexed != 1 {
		t.Fatalf("Indexed = %d, want 1 (secret_excluded must not be indexed)", stats.Indexed)
	}
	if stats.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (secret_excluded must count as skipped)", stats.Skipped)
	}
}

// TestNextPending_SkipsErroredAndDeletedParents pins issue #425 invariant 2:
// NextPending must not hand out chunks whose parent document is status='error'
// or tombstoned, while chunks of an 'ok' document remain visible.
func TestNextPending_SkipsErroredAndDeletedParents(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	okChunk := seedDocWithPendingChunk(ctx, t, st, "docs/ok.md", "ok")
	_ = seedDocWithPendingChunk(ctx, t, st, "docs/broken.md", "error")
	deletedChunk := seedDocWithPendingChunk(ctx, t, st, "docs/gone.md", "ok")
	if err := st.MarkDocumentDeleted(ctx, "docs/gone.md"); err != nil {
		t.Fatalf("MarkDocumentDeleted: %v", err)
	}

	tasks, err := st.NextPending(ctx, 100, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}

	got := make(map[int64]bool, len(tasks))
	for _, task := range tasks {
		got[int64(task.Label)] = true
	}

	if !got[okChunk] {
		t.Fatalf("ok document's chunk %d missing from NextPending: %#v", okChunk, tasks)
	}
	for _, task := range tasks {
		if task.Metadata.RelPath == "docs/broken.md" {
			t.Fatalf("errored document's chunk leaked into NextPending: %#v", task)
		}
	}
	if got[deletedChunk] {
		t.Fatalf("tombstoned document's chunk %d leaked into NextPending", deletedChunk)
	}
}
