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
	listErr  error
	repErr   error
}

func (f *fakeEmbeddedSource) ListEmbeddedChunkMetadata(_ context.Context, kind string, limit int, afterChunkID int64) ([]model.ChunkTask, error) {
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
	return nil
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
