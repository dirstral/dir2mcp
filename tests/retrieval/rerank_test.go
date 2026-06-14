package tests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fakeReranker records the documents it was asked to score and returns
// a caller-supplied ordering (indices into the documents slice).
type fakeReranker struct {
	mu       sync.Mutex
	calls    int
	lastDocs []string
	order    []int // indices, best-first; nil => identity
	err      error
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, _ string, docs []string, topN int) ([]model.Reranked, error) {
	f.mu.Lock()
	f.calls++
	f.lastDocs = append([]string(nil), docs...)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	order := f.order
	if order == nil {
		order = make([]int, len(docs))
		for i := range docs {
			order[i] = i
		}
	}
	out := make([]model.Reranked, 0, len(order))
	for rank, idx := range order {
		if idx < 0 || idx >= len(docs) {
			continue
		}
		out = append(out, model.Reranked{Index: idx, RelevanceScore: float64(len(order) - rank)})
	}
	return out, nil
}

func rerankTestService(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for id, vec := range map[uint64][]float32{1: {1, 0}, 2: {0.9, 0.1}, 3: {0.8, 0.2}} {
		addVec(t, idx, id, vec)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {0, 1},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "beta"})
	svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "c.md", DocType: "md", Snippet: "gamma"})
	return svc
}

func search(t *testing.T, svc *retrieval.Service, k int) []model.SearchHit {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: k, Index: "text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func TestRerank_DisabledLeavesFusedOrder(t *testing.T) {
	svc := rerankTestService(t)
	fr := &fakeReranker{order: []int{2, 1, 0}} // would reverse if consulted
	svc.SetReranker(fr, "m", 50)               // configured but...
	// SetRerankEnabled not called -> disabled by default
	baseline := search(t, svc, 3)

	if fr.calls != 0 {
		t.Fatalf("reranker must not be called when disabled; calls=%d", fr.calls)
	}
	if len(baseline) == 0 {
		t.Fatal("expected hits")
	}
}

func TestRerank_EnabledReordersByProviderScore(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)
	if len(before) < 3 {
		t.Fatalf("need >=3 fused hits, got %d", len(before))
	}
	// Ask the reranker to invert the fused order.
	inv := make([]int, len(before))
	for i := range inv {
		inv[i] = len(before) - 1 - i
	}
	fr := &fakeReranker{order: inv}
	svc.SetReranker(fr, "m", 50)
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	if fr.calls != 1 {
		t.Fatalf("reranker calls=%d, want 1", fr.calls)
	}
	if after[0].ChunkID != before[len(before)-1].ChunkID {
		t.Fatalf("rerank did not reorder: after[0]=%d before-last=%d", after[0].ChunkID, before[len(before)-1].ChunkID)
	}
	// Score must be overwritten with provider relevance, sorted desc.
	for i := 1; i < len(after); i++ {
		if after[i-1].Score < after[i].Score {
			t.Fatalf("not sorted by relevance desc: %v", after)
		}
	}
}

func TestRerank_ProviderErrorFallsBackToFusedOrder(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)

	fr := &fakeReranker{err: errors.New("cohere down"), order: []int{2, 1, 0}}
	svc.SetReranker(fr, "m", 50)
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	if len(after) != len(before) {
		t.Fatalf("len mismatch after fallback: %d vs %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ChunkID != before[i].ChunkID {
			t.Fatalf("fail-open must preserve fused order; pos %d: %d vs %d", i, after[i].ChunkID, before[i].ChunkID)
		}
	}
}

func TestRerank_CandidatePoolCapRespected(t *testing.T) {
	svc := rerankTestService(t)
	fr := &fakeReranker{}
	svc.SetReranker(fr, "m", 2) // cap pool to 2
	svc.SetRerankEnabled(true)

	_ = search(t, svc, 3)
	fr.mu.Lock()
	got := len(fr.lastDocs)
	fr.mu.Unlock()
	if got != 2 {
		t.Fatalf("candidate pool cap not applied: reranker saw %d docs, want 2", got)
	}
}

func TestRerank_FailOpenReturnsFullFusedNotPoolWhenPoolBelowK(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3) // 3 fused hits
	if len(before) != 3 {
		t.Fatalf("need 3 fused hits, got %d", len(before))
	}
	fr := &fakeReranker{err: errors.New("cohere down")}
	svc.SetReranker(fr, "m", 2) // pool < k
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	if len(after) != len(before) {
		t.Fatalf("fail-open with pool<k must still return full fused k: got %d want %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ChunkID != before[i].ChunkID {
			t.Fatalf("fail-open must preserve fused order at %d: %d vs %d", i, after[i].ChunkID, before[i].ChunkID)
		}
	}
}

func TestRerank_SuccessKeepsUnrerankedTailWhenPoolBelowK(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)
	fr := &fakeReranker{order: []int{1, 0}} // invert the 2 pooled docs
	svc.SetReranker(fr, "m", 2)             // pool < k=3
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	if len(after) != 3 {
		t.Fatalf("rerank must not drop results when pool<k: got %d want 3", len(after))
	}
	fr.mu.Lock()
	seen := len(fr.lastDocs)
	fr.mu.Unlock()
	if seen != 2 {
		t.Fatalf("pool cap not applied: reranker saw %d docs, want 2", seen)
	}
	if after[2].ChunkID != before[2].ChunkID {
		t.Fatalf("un-reranked tail must be preserved last: got %d want %d", after[2].ChunkID, before[2].ChunkID)
	}
}

func TestRerank_BothModeReranksOnceOnMergedPool(t *testing.T) {
	// Distinct text and code indices so index=both genuinely merges two
	// non-empty pools; without separate indices the assertion could pass
	// on a single-index result.
	textIdx := index.NewHNSWIndex("")
	addVec(t, textIdx, 1, []float32{1, 0})
	codeIdx := index.NewHNSWIndex("")
	addVec(t, codeIdx, 2, []float32{0, 1})
	svc := retrieval.NewService(nil, textIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0}, // text query -> matches id 1 in textIdx
		"codestral-embed": {0, 1}, // code query -> matches id 2 in codeIdx
	}}, nil)
	svc.SetCodeIndex(codeIdx)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.go", DocType: "code", Snippet: "beta"})

	fr := &fakeReranker{}
	svc.SetReranker(fr, "m", 50)
	svc.SetRerankEnabled(true)

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 5, Index: "both"}); err != nil {
		t.Fatalf("Search both: %v", err)
	}
	if fr.calls != 1 {
		t.Fatalf("index=both must rerank exactly once on the merged pool; calls=%d", fr.calls)
	}
	// The single rerank call must see the merged pool: hits from both
	// the text index (alpha) and the code index (beta).
	fr.mu.Lock()
	docs := append([]string(nil), fr.lastDocs...)
	fr.mu.Unlock()
	seen := map[string]bool{}
	for _, d := range docs {
		seen[d] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("reranker must see the merged two-index pool; saw docs=%v", docs)
	}
}
