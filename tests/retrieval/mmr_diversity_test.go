package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// newMMRService builds a vector-only retrieval service (nil store keeps hybrid
// fusion from engaging, so each hit's Score is the raw cosine similarity) with
// three candidates whose cosine scores against the query vector {1, 0} are:
//
//	chunk 1 → 1.00, snippet "alpha apple fruit"        (most relevant)
//	chunk 2 → ~0.92, snippet "alpha apple fruit"       (near-duplicate of 1)
//	chunk 3 → 0.80,  snippet "zebra ocean mountain"    (diverse, less relevant)
//
// Pure relevance order is [1, 2, 3]. MMR with a balanced lambda demotes the
// near-duplicate (chunk 2) below the diverse hit (chunk 3): [1, 3, 2].
func newMMRService(t *testing.T, enabled bool, lambda float64) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})       // cosine 1.00
	addVec(t, idx, 2, []float32{0.92, 0.39}) // cosine ~0.92
	addVec(t, idx, 3, []float32{0.80, 0.60}) // cosine 0.80 (unit vector)

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha apple fruit"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "b.md", Snippet: "alpha apple fruit"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "c.md", Snippet: "zebra ocean mountain"})
	svc.SetMMR(enabled, lambda)
	return svc
}

func mmrSearchChunkIDs(t *testing.T, svc *retrieval.Service, k int) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: k})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

func assertOrder(t *testing.T, got, want []uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order length mismatch: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch: want %v, got %v", want, got)
		}
	}
}

// TestSearch_MMR_DemotesNearDuplicate pins the core behavior: with MMR enabled
// and a balanced lambda, the near-duplicate (chunk 2) is demoted below the
// diverse hit (chunk 3), surfacing distinct evidence higher.
func TestSearch_MMR_DemotesNearDuplicate(t *testing.T) {
	svc := newMMRService(t, true, 0.5)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{1, 3, 2})
}

// TestSearch_MMR_DisabledKeepsRelevanceOrder pins the default-off behavior:
// with MMR disabled the candidate order is the unchanged pure-relevance order.
func TestSearch_MMR_DisabledKeepsRelevanceOrder(t *testing.T) {
	svc := newMMRService(t, false, 0.5)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{1, 2, 3})
}

// TestSearch_MMR_LambdaOneIsPureRelevance pins that lambda=1 disables the
// diversity penalty entirely, so MMR reproduces the pure-relevance order even
// when enabled.
func TestSearch_MMR_LambdaOneIsPureRelevance(t *testing.T) {
	svc := newMMRService(t, true, 1.0)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{1, 2, 3})
}

// TestSearch_MMR_LambdaZeroPrefersDiversity pins the opposite extreme: lambda=0
// ignores relevance, so after the (relevance-seeded) first pick the most
// dissimilar candidate is preferred. Chunk 1 is selected first (highest
// relevance, no prior selections), then chunk 3 (sim 0 to chunk 1) beats chunk
// 2 (sim 1 to chunk 1).
func TestSearch_MMR_LambdaZeroPrefersDiversity(t *testing.T) {
	svc := newMMRService(t, true, 0)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{1, 3, 2})
}

// TestSearch_MMR_LambdaZeroSeedsByRelevanceNotChunkID locks in that the FIRST
// MMR pick is relevance-seeded even when lambda=0. The most relevant hit here is
// chunk 5 (cosine 1.00) while the lowest ChunkID is chunk 1 (cosine 0.80, a
// diverse snippet). If the first selection applied the full objective it would
// tie all candidates at 0 (maxSim=0, lambda=0) and the ChunkID tiebreak would
// wrongly seed with chunk 1. The relevance-seeded pick selects chunk 5 first;
// then chunk 1 (sim 0 to chunk 5) beats the near-duplicate chunk 2 (sim 1).
func TestSearch_MMR_LambdaZeroSeedsByRelevanceNotChunkID(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{0.80, 0.60}) // cosine 0.80, lowest ChunkID
	addVec(t, idx, 2, []float32{0.92, 0.39}) // cosine ~0.92, near-duplicate of 5
	addVec(t, idx, 5, []float32{1, 0})       // cosine 1.00, most relevant
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "zebra ocean mountain"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "b.md", Snippet: "alpha apple fruit"})
	svc.SetChunkMetadata(5, model.SearchHit{RelPath: "c.md", Snippet: "alpha apple fruit"})
	svc.SetMMR(true, 0)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{5, 1, 2})
}

// TestSearch_MMR_ComposesWithRerank pins that MMR runs as the LAST reordering
// step, after the rerank stage and before the final truncation: the reranker
// scores the pool in pure-relevance order (best-first 1,2,3) and MMR then
// diversifies it to [1, 3, 2], demoting the near-duplicate. This exercises the
// rerankPool path (the universal final step) rather than the rerank-disabled
// fast path covered by the other cases.
func TestSearch_MMR_ComposesWithRerank(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.92, 0.39})
	addVec(t, idx, 3, []float32{0.80, 0.60})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", Snippet: "alpha apple fruit"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", Snippet: "alpha apple fruit"})
	svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "c.md", Snippet: "zebra ocean mountain"})
	// Identity rerank (best-first 1,2,3) with descending scores: the post-rerank
	// order is the pure-relevance order MMR then diversifies.
	svc.SetReranker(&fakeReranker{order: []int{0, 1, 2}}, "", 0)
	svc.SetRerankEnabled(true)
	svc.SetMMR(true, 0.5)

	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{1, 3, 2})
}

// TestSearch_MMR_SingleHitIsStable pins that a single-candidate pool is returned
// unchanged regardless of MMR (no reordering is possible / meaningful).
func TestSearch_MMR_SingleHitIsStable(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 7, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(7, model.SearchHit{RelPath: "x.md", Snippet: "solo"})
	svc.SetMMR(true, 0.5)
	got := mmrSearchChunkIDs(t, svc, 10)
	assertOrder(t, got, []uint64{7})
}
