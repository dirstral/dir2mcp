package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedFailedChunk inserts one chunk for relPath and parks it in
// embedding_status='error' with the given category, the exact state a
// provider-side embed rejection leaves behind.
func seedFailedChunk(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath, category string) uint64 {
	t.Helper()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "filesystem", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument(%s): %v", relPath, err)
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
		}, nil)
		chunkID = id
		return cerr
	})
	if err != nil {
		t.Fatalf("seed chunk for %s: %v", relPath, err)
	}
	if err := st.MarkFailedWithCategory(ctx, []uint64{uint64(chunkID)}, category, "provider rejected the request"); err != nil {
		t.Fatalf("MarkFailedWithCategory(%s): %v", relPath, err)
	}
	return uint64(chunkID)
}

// pendingChunkIDs returns the chunk ids the embed worker would pick up next.
func pendingChunkIDs(t *testing.T, ctx context.Context, st *store.SQLiteStore) map[uint64]struct{} {
	t.Helper()
	tasks, err := st.NextPending(ctx, 100, "")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	out := map[uint64]struct{}{}
	for _, task := range tasks {
		out[task.Label] = struct{}{}
	}
	return out
}

// TestRequeueFailedChunks_RetryableOnly is the core of issue #783: a chunk that
// a provider rejected for a provider-side reason (a revoked key) can be put
// back in the embed queue WITHOUT re-ingesting its document, while a failure
// that is a property of the stored input stays terminal.
func TestRequeueFailedChunks_RetryableOnly(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	authChunk := seedFailedChunk(t, ctx, st, "auth-failed.md", "auth")
	oversizeChunk := seedFailedChunk(t, ctx, st, "too-large.md", "payload_too_large")

	// Nothing is pending: both chunks are parked in 'error'.
	if pending := pendingChunkIDs(t, ctx, st); len(pending) != 0 {
		t.Fatalf("expected no pending chunks before the retry, got %v", pending)
	}

	requeued, err := st.RequeueFailedChunks(ctx, store.RequeueableErrorCategories())
	if err != nil {
		t.Fatalf("RequeueFailedChunks: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued %d chunk(s), want 1 (only the auth failure is retryable)", requeued)
	}

	pending := pendingChunkIDs(t, ctx, st)
	if _, ok := pending[authChunk]; !ok {
		t.Errorf("auth-failed chunk %d was not returned to the embed queue (pending=%v)", authChunk, pending)
	}
	if _, ok := pending[oversizeChunk]; ok {
		t.Errorf("payload_too_large chunk %d was requeued; terminal categories must stay terminal", oversizeChunk)
	}

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.EmbeddedPending != 1 {
		t.Errorf("embedded_pending=%d, want 1", stats.EmbeddedPending)
	}
	if stats.FailureSummary == nil || stats.FailureSummary.Categories["payload_too_large"] != 1 {
		t.Errorf("payload_too_large should still be reported as failed, got %+v", stats.FailureSummary)
	}
	if stats.FailureSummary != nil && stats.FailureSummary.Categories["auth"] != 0 {
		t.Errorf("auth failures should be cleared after the retry, got %+v", stats.FailureSummary)
	}
}

// TestRequeueFailedChunks_UnclassifiedFailuresAreRecoverable pins the decision
// that "unknown" (which includes every failure recorded through the
// unclassified MarkFailed entry point) belongs to the retryable set. Excluding
// it would strand that whole class permanently, which is the bug #783 is about.
func TestRequeueFailedChunks_UnclassifiedFailuresAreRecoverable(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chunkID := seedFailedChunk(t, ctx, st, "unclassified.md", "")

	requeued, err := st.RequeueFailedChunks(ctx, store.RequeueableErrorCategories())
	if err != nil {
		t.Fatalf("RequeueFailedChunks: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued %d chunk(s), want 1", requeued)
	}
	if _, ok := pendingChunkIDs(t, ctx, st)[chunkID]; !ok {
		t.Errorf("chunk %d with an empty error_category was not requeued", chunkID)
	}
}

// TestRequeueFailedChunks_ExplicitCategoryFilter pins the --error-category
// path: only the named category moves, everything else is left exactly where
// it was.
func TestRequeueFailedChunks_ExplicitCategoryFilter(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	authChunk := seedFailedChunk(t, ctx, st, "auth.md", "auth")
	rateChunk := seedFailedChunk(t, ctx, st, "rate.md", "rate_limit")

	requeued, err := st.RequeueFailedChunks(ctx, []string{"rate_limit"})
	if err != nil {
		t.Fatalf("RequeueFailedChunks: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued %d chunk(s), want 1", requeued)
	}
	pending := pendingChunkIDs(t, ctx, st)
	if _, ok := pending[rateChunk]; !ok {
		t.Errorf("rate_limit chunk %d was not requeued", rateChunk)
	}
	if _, ok := pending[authChunk]; ok {
		t.Errorf("auth chunk %d was requeued despite --error-category=rate_limit", authChunk)
	}

	// An empty selection is a no-op rather than a corpus-wide reset.
	if n, err := st.RequeueFailedChunks(ctx, nil); err != nil || n != 0 {
		t.Errorf("RequeueFailedChunks(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestFailureSummary_CarriesFailureAge pins the reporting half of #783: the
// failure aggregate says WHEN the failures it reports happened, so a set
// stranded by an earlier run cannot read as one this run just produced (the
// corpus.json `ts` is stamped at write time and cannot make that distinction).
func TestFailureSummary_CarriesFailureAge(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	before := time.Now().UTC().Add(-2 * time.Second).Unix()
	seedFailedChunk(t, ctx, st, "auth.md", "auth")
	after := time.Now().UTC().Add(2 * time.Second).Unix()

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.FailureSummary == nil {
		t.Fatalf("expected a failure summary")
	}
	if stats.FailureSummary.LastFailureUnix < before || stats.FailureSummary.LastFailureUnix > after {
		t.Errorf("last_failure_unix=%d outside [%d,%d]", stats.FailureSummary.LastFailureUnix, before, after)
	}
	if len(stats.FailureSummary.Samples) != 1 || stats.FailureSummary.Samples[0].FailedUnix == 0 {
		t.Errorf("expected the sample to carry its own failure timestamp, got %+v", stats.FailureSummary.Samples)
	}

	// The stamp must not outlive the failure: a requeued chunk is no longer
	// failed, so the aggregate must stop reporting a failure age at all.
	if _, err := st.RequeueFailedChunks(ctx, []string{"auth"}); err != nil {
		t.Fatalf("RequeueFailedChunks: %v", err)
	}
	cleared, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats after retry: %v", err)
	}
	if cleared.FailureSummary != nil {
		t.Errorf("failure summary should be empty after the retry, got %+v", cleared.FailureSummary)
	}
}
