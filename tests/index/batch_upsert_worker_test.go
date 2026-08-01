package tests

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #429 F8: EmbeddingWorker.indexChunks upserts one vector per chunk, which
// on the on-disk backend meant one fsync per chunk. The worker now type-asserts
// the index against the optional model.BatchUpserter and applies the whole batch
// in one call, falling back to the per-chunk loop for backends that do not
// implement it (memory/Qdrant/pgvector) and for batch failures.

// batchIndex is a model.Index that records how the worker dispatched its
// upserts. batchErr, when set, fails BatchUpsert; upsertErrFor fails the
// per-chunk Upsert for one chunk ID so the fallback's error attribution can be
// asserted.
type batchIndex struct {
	batches      [][]model.IndexUpsert
	upserted     []uint64
	batchErr     error
	upsertErrFor uint64
}

func (b *batchIndex) BatchUpsert(_ context.Context, items []model.IndexUpsert) error {
	if b.batchErr != nil {
		return b.batchErr
	}
	cp := make([]model.IndexUpsert, len(items))
	copy(cp, items)
	b.batches = append(b.batches, cp)
	return nil
}

func (b *batchIndex) Upsert(_ context.Context, _ []float32, payload model.IndexPayload) error {
	if b.upsertErrFor != 0 && payload.ChunkID == b.upsertErrFor {
		return errors.New("upsert rejected")
	}
	b.upserted = append(b.upserted, payload.ChunkID)
	return nil
}

func (b *batchIndex) Delete(_ context.Context, _ []uint64) error { return nil }
func (b *batchIndex) Search(_ context.Context, _ []float32, _ int, _ model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (b *batchIndex) Identity(_ context.Context) (string, error) { return "", nil }
func (b *batchIndex) Reset(_ context.Context, _ string) error    { return nil }
func (b *batchIndex) Close() error                               { return nil }

// serialIndex is a model.Index WITHOUT BatchUpsert: the fallback path.
type serialIndex struct{ upserted []uint64 }

func (s *serialIndex) Upsert(_ context.Context, _ []float32, payload model.IndexPayload) error {
	s.upserted = append(s.upserted, payload.ChunkID)
	return nil
}
func (s *serialIndex) Delete(_ context.Context, _ []uint64) error { return nil }
func (s *serialIndex) Search(_ context.Context, _ []float32, _ int, _ model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (s *serialIndex) Identity(_ context.Context) (string, error) { return "", nil }
func (s *serialIndex) Reset(_ context.Context, _ string) error    { return nil }
func (s *serialIndex) Close() error                               { return nil }

func batchTasks(ids ...uint64) []model.ChunkTask {
	tasks := make([]model.ChunkTask, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, model.NewChunkTask(id, "text", "text", model.ChunkMetadata{ChunkID: id, RelPath: "a.txt"}))
	}
	return tasks
}

func unitVectors(n int) [][]float32 {
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = []float32{float32(i + 1), 1}
	}
	return vecs
}

// TestIndexChunks_UsesBatchUpsertWhenAvailable pins that a BatchUpserter backend
// receives ONE batch containing every chunk (not one Upsert per chunk), that the
// payloads are the same ones the per-chunk path builds, and that OnIndexedChunk
// still fires once per chunk in task order.
func TestIndexChunks_UsesBatchUpsertWhenAvailable(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := &batchIndex{}
	var indexed []uint64
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: &fakeEmbedder{vectors: unitVectors(3)}, BatchSize: 8,
		OnIndexedChunk: func(label uint64, _ model.ChunkMetadata) { indexed = append(indexed, label) },
	}

	n, err := worker.EmbedAndIndex(ctx, "text", batchTasks(11, 22, 33))
	if err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed = %d, want 3", n)
	}
	if len(idx.batches) != 1 {
		t.Fatalf("BatchUpsert called %d times, want exactly 1 for a 3-chunk batch", len(idx.batches))
	}
	if len(idx.upserted) != 0 {
		t.Fatalf("per-chunk Upsert also called for %v; the batch path must replace it", idx.upserted)
	}
	got := make([]uint64, 0, 3)
	for _, item := range idx.batches[0] {
		got = append(got, item.Payload.ChunkID)
		if item.Payload.RelPath != "a.txt" {
			t.Fatalf("batched payload lost fields: %+v", item.Payload)
		}
		if len(item.Vector) == 0 {
			t.Fatalf("batched item %d carries no vector", item.Payload.ChunkID)
		}
	}
	if len(got) != 3 || got[0] != 11 || got[1] != 22 || got[2] != 33 {
		t.Fatalf("batched chunk ids = %v, want [11 22 33] in task order", got)
	}
	if len(indexed) != 3 {
		t.Fatalf("OnIndexedChunk fired %d times, want 3", len(indexed))
	}
	if len(src.embedded) != 3 {
		t.Fatalf("embedded = %v, want all three marked", src.embedded)
	}
}

// TestIndexChunks_FallsBackWithoutBatchUpsert pins that a backend that does NOT
// implement model.BatchUpserter keeps the existing per-chunk behavior.
func TestIndexChunks_FallsBackWithoutBatchUpsert(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := &serialIndex{}
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: &fakeEmbedder{vectors: unitVectors(2)}, BatchSize: 8,
	}

	n, err := worker.EmbedAndIndex(ctx, "text", batchTasks(11, 22))
	if err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2", n)
	}
	if len(idx.upserted) != 2 || idx.upserted[0] != 11 || idx.upserted[1] != 22 {
		t.Fatalf("upserted = %v, want [11 22] via the per-chunk fallback", idx.upserted)
	}
	if len(src.embedded) != 2 {
		t.Fatalf("embedded = %v, want both marked", src.embedded)
	}
}

// TestIndexChunks_BatchFailureReplaysPerChunk pins the partial-failure contract:
// when the batch call fails, the worker replays per chunk so the offending chunk
// is still attributed — healthy predecessors are marked embedded and only the
// bad chunk is marked failed with its category.
func TestIndexChunks_BatchFailureReplaysPerChunk(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := &batchIndex{batchErr: errors.New("batch write failed"), upsertErrFor: 22}
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: &fakeEmbedder{vectors: unitVectors(3)}, BatchSize: 8,
	}

	n, err := worker.EmbedAndIndex(ctx, "text", batchTasks(11, 22, 33))
	if err == nil {
		t.Fatal("EmbedAndIndex must surface the per-chunk index error")
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1 (chunk 11 landed before chunk 22 failed)", n)
	}
	if len(idx.upserted) != 1 || idx.upserted[0] != 11 {
		t.Fatalf("replayed upserts = %v, want [11] before the failure", idx.upserted)
	}
	if len(src.embedded) != 1 || src.embedded[0] != 11 {
		t.Fatalf("embedded = %v, want [11] marked before the index error", src.embedded)
	}
	if len(src.failedLabels) != 1 || src.failedLabels[0] != 22 {
		t.Fatalf("failed = %v, want exactly the offending chunk [22]", src.failedLabels)
	}
	if src.failedCategory == "" {
		t.Fatal("failed chunk lost its error category")
	}
}

// TestEmbedAndIndex_DiskBackendBatchesEndToEnd wires the real disk backend to the
// worker and pins that the batch path leaves the segment durable and searchable —
// including after a reopen, so the single per-batch fsync really persisted every
// vector.
func TestEmbedAndIndex_DiskBackendBatchesEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), diskindex.SegmentFileName("text"))
	idx := diskindex.New(path)
	src := &fakeChunkSource{}
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: &fakeEmbedder{vectors: unitVectors(3)}, BatchSize: 8,
	}

	if _, err := worker.EmbedAndIndex(ctx, "text", batchTasks(11, 22, 33)); err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := diskindex.New(path)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	hits, err := reopened.Search(ctx, []float32{2, 1}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits after reopen = %d, want all 3 batched vectors durable", len(hits))
	}
}
