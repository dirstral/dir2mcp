package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// rawReranker returns exactly the indices it is given, without the sanity
// filtering fakeReranker applies. It models a provider that answers HTTP 200
// with a malformed or partial result set: fewer indices than candidates, a
// repeated index, or an index outside the pool (issue #669).
type rawReranker struct {
	mu       sync.Mutex
	calls    int
	lastDocs []string
	indices  []int
}

func (r *rawReranker) Rerank(_ context.Context, _ string, _ string, docs []string, _ int) ([]model.Reranked, error) {
	r.mu.Lock()
	r.calls++
	r.lastDocs = append([]string(nil), docs...)
	r.mu.Unlock()
	out := make([]model.Reranked, 0, len(r.indices))
	for rank, idx := range r.indices {
		out = append(out, model.Reranked{Index: idx, RelevanceScore: float64(len(r.indices) - rank)})
	}
	return out, nil
}

// chunkIDs collects the chunk ids of hits in result order.
func chunkIDs(hits []model.SearchHit) []uint64 {
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

// assertNoLossNoDuplicate checks the two invariants of SPEC 9.1.1 "no result
// loss": every pre-rerank hit still appears, and no hit appears twice.
func assertNoLossNoDuplicate(t *testing.T, before, after []model.SearchHit) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("rerank changed the result count: got %d (%v) want %d (%v)",
			len(after), chunkIDs(after), len(before), chunkIDs(before))
	}
	seen := map[uint64]int{}
	for _, h := range after {
		seen[h.ChunkID]++
		if seen[h.ChunkID] > 1 {
			t.Fatalf("rerank duplicated chunk %d: got %v", h.ChunkID, chunkIDs(after))
		}
	}
	for _, h := range before {
		if seen[h.ChunkID] == 0 {
			t.Fatalf("rerank dropped chunk %d: got %v want the set %v",
				h.ChunkID, chunkIDs(after), chunkIDs(before))
		}
	}
}

// A provider that scores only part of the pool must not drop the rest. The
// unscored candidates are appended after the reranked ones in fused order.
func TestRerank_PartialResultSetKeepsUnscoredCandidates(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)
	if len(before) != 3 {
		t.Fatalf("need 3 fused hits, got %d", len(before))
	}

	rr := &rawReranker{indices: []int{1, 0}} // 3 candidates sent, only 2 scored
	svc.SetReranker(rr, "m", 50)
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	if rr.calls != 1 {
		t.Fatalf("reranker calls=%d, want 1", rr.calls)
	}
	rr.mu.Lock()
	sent := len(rr.lastDocs)
	rr.mu.Unlock()
	if sent != 3 {
		t.Fatalf("reranker must see the whole pool: saw %d docs, want 3", sent)
	}
	assertNoLossNoDuplicate(t, before, after)
	// The two scored candidates were inverted, so they lead in inverted order
	// and the unscored candidate lands last.
	if after[0].ChunkID != before[1].ChunkID {
		t.Fatalf("scored candidates must lead in provider order: got %v", chunkIDs(after))
	}
	if after[2].ChunkID != before[2].ChunkID {
		t.Fatalf("unscored candidate must be appended last in fused order: got %v", chunkIDs(after))
	}
}

// A repeated index makes the response an invalid ranking. It must never emit
// the same hit twice; the query falls open to the fused order.
func TestRerank_DuplicateIndexFallsOpenWithoutDuplicatingHits(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)
	if len(before) != 3 {
		t.Fatalf("need 3 fused hits, got %d", len(before))
	}

	rr := &rawReranker{indices: []int{0, 0, 1}}
	svc.SetReranker(rr, "m", 50)
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	assertNoLossNoDuplicate(t, before, after)
	for i := range before {
		if after[i].ChunkID != before[i].ChunkID {
			t.Fatalf("fail-open must preserve fused order at %d: got %v want %v",
				i, chunkIDs(after), chunkIDs(before))
		}
	}
}

// A mixed response (valid plus out-of-range indices) is rejected whole, the
// same policy the provider-error path uses.
func TestRerank_MixedValidAndOutOfRangeFallsOpen(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)

	rr := &rawReranker{indices: []int{0, 7, 1}}
	svc.SetReranker(rr, "m", 50)
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	assertNoLossNoDuplicate(t, before, after)
	for i := range before {
		if after[i].ChunkID != before[i].ChunkID {
			t.Fatalf("fail-open must preserve fused order at %d: got %v want %v",
				i, chunkIDs(after), chunkIDs(before))
		}
	}
}

// A partial response with the pool capped below k must keep both the unscored
// pool members and the fused tail beyond the pool.
func TestRerank_PartialResultSetWithPoolBelowKKeepsEveryHit(t *testing.T) {
	svc := rerankTestService(t)
	before := search(t, svc, 3)
	if len(before) != 3 {
		t.Fatalf("need 3 fused hits, got %d", len(before))
	}

	rr := &rawReranker{indices: []int{1}} // 2 candidates sent, only 1 scored
	svc.SetReranker(rr, "m", 2)           // pool < k
	svc.SetRerankEnabled(true)

	after := search(t, svc, 3)
	assertNoLossNoDuplicate(t, before, after)
	if after[0].ChunkID != before[1].ChunkID {
		t.Fatalf("the scored candidate must lead: got %v", chunkIDs(after))
	}
	if after[2].ChunkID != before[2].ChunkID {
		t.Fatalf("the fused tail beyond the pool must stay last: got %v", chunkIDs(after))
	}
}
