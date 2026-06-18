package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestEmbedAndIndex_MatchesRunOnce pins that the EmbedAndIndex step extracted for
// the distributed embed-worker (SPEC §8.7) produces the same result as the
// in-process RunOnce path: the vector is indexed and the chunk is marked
// embedded. This is the "distributed-mode-off = existing behavior unchanged"
// invariant at the shared-step level — both paths run identical embed/index/mark
// logic.
func TestEmbedAndIndex_MatchesRunOnce(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &fakeEmbedder{vectors: [][]float32{{0.6, 0.8}}}
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: emb, BatchSize: 4,
	}

	task := model.NewChunkTask(11, "hello", "text", model.ChunkMetadata{ChunkID: 11, RelPath: "a.txt"})
	n, err := worker.EmbedAndIndex(ctx, "text", []model.ChunkTask{task})
	if err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}
	if len(src.embedded) != 1 || src.embedded[0] != 11 {
		t.Fatalf("embedded = %v, want [11]", src.embedded)
	}

	hits, err := idx.Search(ctx, []float32{0.6, 0.8}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 11 {
		t.Fatalf("hits = %+v, want exactly chunk 11", hits)
	}
}

// TestEmbedAndIndex_IdempotentNoDuplicateVectors pins SPEC §8.7.3: re-running an
// embedding job for the same chunk_id (at-least-once / redelivery) overwrites the
// same vector — it does NOT create a duplicate. After embedding the same chunk
// twice, a search returns exactly one hit for it.
func TestEmbedAndIndex_IdempotentNoDuplicateVectors(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &fakeEmbedder{vectors: [][]float32{{1, 0}}}
	worker := &index.EmbeddingWorker{
		Source: src, Index: idx, Embedder: emb, BatchSize: 4,
	}

	task := model.NewChunkTask(21, "dup", "text", model.ChunkMetadata{ChunkID: 21, RelPath: "b.txt"})

	for i := 0; i < 2; i++ {
		if _, err := worker.EmbedAndIndex(ctx, "text", []model.ChunkTask{task}); err != nil {
			t.Fatalf("EmbedAndIndex pass %d: %v", i+1, err)
		}
	}

	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	count := 0
	for _, h := range hits {
		if h.ChunkID == 21 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("chunk 21 appears %d times after re-embed; want exactly 1 (no duplicate vectors)", count)
	}
}
