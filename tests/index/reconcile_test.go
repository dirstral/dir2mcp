package tests

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakeEmbeddedSource records the embedded chunks per kind and the labels
// re-pended by reconciliation. It paginates over the recorded set so
// ReconcileEmbeddedVectors exercises its paging loop.
type fakeEmbeddedSource struct {
	byKind   map[string][]uint64
	repended []uint64
	// rependBatches records the size of each RependEmbeddedChunks call so tests
	// can assert re-pending is flushed in bounded batches (issue #503) rather than
	// one terminal call.
	rependBatches []int
	// seeks records the afterChunkID cursor passed to each ListEmbeddedChunkMetadata
	// call so tests can assert the keyset walk advances monotonically.
	seeks   []int64
	listErr error
	repErr  error
}

func (f *fakeEmbeddedSource) ListEmbeddedChunkMetadata(_ context.Context, kind string, limit int, afterChunkID int64) ([]model.ChunkTask, error) {
	f.seeks = append(f.seeks, afterChunkID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Mirror the store's keyset semantics: return Labels strictly greater than
	// afterChunkID, ordered ascending, capped at limit.
	ids := append([]uint64(nil), f.byKind[kind]...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]model.ChunkTask, 0, limit)
	for _, id := range ids {
		if int64(id) <= afterChunkID {
			continue
		}
		out = append(out, model.ChunkTask{Label: id, IndexKind: kind})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeEmbeddedSource) RependEmbeddedChunks(_ context.Context, labels []uint64) error {
	if f.repErr != nil {
		return f.repErr
	}
	f.repended = append(f.repended, labels...)
	f.rependBatches = append(f.rependBatches, len(labels))
	return nil
}

// absentIndex is a model.Index that implements index.VectorPresence and reports
// every chunk as missing, standing in for a snapshot that lost all its vectors.
// It lets the batching test drive a large missing set without materializing a
// real HNSW graph.
type absentIndex struct{ model.Index }

func (absentIndex) HasVectors(_ context.Context, chunkIDs []uint64) (map[uint64]bool, error) {
	present := make(map[uint64]bool, len(chunkIDs))
	for _, id := range chunkIDs {
		present[id] = false
	}
	return present, nil
}

// stubIndex is a minimal model.Index that deliberately does NOT implement
// index.VectorPresence, standing in for a durable backend.
type stubIndex struct{ model.Index }

func mustUpsertChunk(t *testing.T, ix *index.HNSWIndex, id uint64) {
	t.Helper()
	if err := ix.Upsert(context.Background(), []float32{1, 0, 0}, model.IndexPayload{ChunkID: id}); err != nil {
		t.Fatalf("upsert %d: %v", id, err)
	}
}

// TestReconcileEmbeddedVectors_RependsMissing pins the #402 A2 fix: chunks that
// sqlite records embedded but whose vectors are absent from a crash-recovered
// in-memory index are re-pended so the embed worker re-embeds them.
func TestReconcileEmbeddedVectors_RependsMissing(t *testing.T) {
	ix := index.NewHNSWIndex("")
	// Chunks 1 and 2 have vectors; chunk 3 was "embedded" per sqlite but its
	// vector was lost to a crash before the snapshot.
	mustUpsertChunk(t, ix, 1)
	mustUpsertChunk(t, ix, 2)

	src := &fakeEmbeddedSource{byKind: map[string][]uint64{index.KindText: {1, 2, 3}}}

	n, err := index.ReconcileEmbeddedVectors(context.Background(), src, ix, index.KindText)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-pended count = %d, want 1", n)
	}
	if len(src.repended) != 1 || src.repended[0] != 3 {
		t.Fatalf("re-pended labels = %v, want [3]", src.repended)
	}
}

// TestReconcileEmbeddedVectors_AllPresentNoop verifies no re-pend happens when
// every embedded chunk's vector is present.
func TestReconcileEmbeddedVectors_AllPresentNoop(t *testing.T) {
	ix := index.NewHNSWIndex("")
	mustUpsertChunk(t, ix, 10)
	mustUpsertChunk(t, ix, 11)
	src := &fakeEmbeddedSource{byKind: map[string][]uint64{index.KindText: {10, 11}}}

	n, err := index.ReconcileEmbeddedVectors(context.Background(), src, ix, index.KindText)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 || len(src.repended) != 0 {
		t.Fatalf("expected no re-pend, got n=%d repended=%v", n, src.repended)
	}
}

// TestReconcileEmbeddedVectors_DurableBackendNoop verifies that a backend which
// does not implement VectorPresence (durable: disk/qdrant/pgvector) is skipped
// entirely — the store is never consulted.
func TestReconcileEmbeddedVectors_DurableBackendNoop(t *testing.T) {
	src := &fakeEmbeddedSource{
		byKind:  map[string][]uint64{index.KindText: {1, 2, 3}},
		listErr: errors.New("store must not be consulted for a durable backend"),
	}
	n, err := index.ReconcileEmbeddedVectors(context.Background(), src, stubIndex{}, index.KindText)
	if err != nil {
		t.Fatalf("reconcile durable: %v", err)
	}
	if n != 0 || len(src.repended) != 0 {
		t.Fatalf("expected durable backend no-op, got n=%d repended=%v", n, src.repended)
	}
}

// TestReconcileEmbeddedVectors_RependsInBoundedBatches pins the #503 fix: a large
// missing set is re-pended across multiple bounded batches as the scan proceeds
// (not one terminal O(total) call), and the union of every batch equals the full
// missing set — so bounding peak memory preserves correctness.
func TestReconcileEmbeddedVectors_RependsInBoundedBatches(t *testing.T) {
	// Enough embedded chunks — all reported missing — that the buffer crosses the
	// re-pend batch threshold several times, forcing multiple flushes.
	const total = 2500
	want := make([]uint64, 0, total)
	for id := uint64(1); id <= total; id++ {
		want = append(want, id)
	}
	src := &fakeEmbeddedSource{byKind: map[string][]uint64{index.KindText: want}}

	n, err := index.ReconcileEmbeddedVectors(context.Background(), src, absentIndex{}, index.KindText)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != total {
		t.Fatalf("re-pended count = %d, want %d", n, total)
	}
	// Bounded batching: more than one flush, and no single flush carried the whole
	// missing set (peak buffer is bounded well below total on a large corpus).
	if len(src.rependBatches) < 2 {
		t.Fatalf("expected re-pend to be flushed in multiple batches, got %d call(s): %v",
			len(src.rependBatches), src.rependBatches)
	}
	sum := 0
	for i, size := range src.rependBatches {
		if size == 0 {
			t.Fatalf("batch %d was empty: %v", i, src.rependBatches)
		}
		if size >= total {
			t.Fatalf("batch %d size %d not bounded below total %d: %v", i, size, total, src.rependBatches)
		}
		sum += size
	}
	if sum != total {
		t.Fatalf("batch sizes sum to %d, want %d: %v", sum, total, src.rependBatches)
	}
	// The keyset cursor is monotonic non-decreasing across pages — each fetch seeks
	// strictly past the previous page's largest chunk_id, so no row is scanned twice
	// and the flush invariant (buffered ID chunk_id <= afterChunkID) can hold.
	for i := 1; i < len(src.seeks); i++ {
		if src.seeks[i] <= src.seeks[i-1] {
			t.Fatalf("seek cursor not monotonic at page %d: %v", i, src.seeks)
		}
	}
	// Correctness preserved: the union of all batches is exactly the missing set.
	got := append([]uint64(nil), src.repended...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != len(want) {
		t.Fatalf("re-pended %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("re-pended set diverges at %d: got %d want %d", i, got[i], want[i])
		}
	}
}

// TestHNSWHasVectors_ReportsPresence pins the membership primitive used by
// reconciliation.
func TestHNSWHasVectors_ReportsPresence(t *testing.T) {
	ix := index.NewHNSWIndex("")
	mustUpsertChunk(t, ix, 7)
	present, err := ix.HasVectors(context.Background(), []uint64{7, 8})
	if err != nil {
		t.Fatalf("HasVectors: %v", err)
	}
	if !present[7] {
		t.Fatalf("chunk 7 should be present")
	}
	if present[8] {
		t.Fatalf("chunk 8 should be absent")
	}
}
