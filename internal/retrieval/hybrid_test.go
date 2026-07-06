package retrieval

import (
	"math"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

func TestFuseRRF_DisjointLists(t *testing.T) {
	primary := []model.SearchHit{
		{ChunkID: 1, Snippet: "p1"},
		{ChunkID: 2, Snippet: "p2"},
	}
	secondary := []model.SearchHit{
		{ChunkID: 3, Snippet: "s3"},
		{ChunkID: 4, Snippet: "s4"},
	}
	out := fuseRRF(primary, secondary, 10)
	if len(out) != 4 {
		t.Fatalf("expected 4 fused hits, got %d", len(out))
	}
	// Rank-1 contributions are equal across lists, so chunks 1 and 3 should
	// share the top score, then chunks 2 and 4. Verify decreasing order.
	for i := 1; i < len(out); i++ {
		if out[i].Score > out[i-1].Score {
			t.Fatalf("not sorted by score: %v", scores(out))
		}
	}
	want := 1.0 / float64(rrfK+1)
	if math.Abs(out[0].Score-want) > 1e-9 {
		t.Errorf("top score: got %v want %v", out[0].Score, want)
	}
}

func TestFuseRRF_OverlapBoostsSharedHit(t *testing.T) {
	// Chunk 42 appears at rank 1 in BOTH lists; its fused score should be
	// strictly greater than any chunk that appears in only one list.
	primary := []model.SearchHit{
		{ChunkID: 42, Snippet: "shared"},
		{ChunkID: 1, Snippet: "p-only"},
	}
	secondary := []model.SearchHit{
		{ChunkID: 42, Snippet: "shared"},
		{ChunkID: 2, Snippet: "s-only"},
	}
	out := fuseRRF(primary, secondary, 10)
	if out[0].ChunkID != 42 {
		t.Fatalf("expected chunk 42 to win the fusion, got chunk %d", out[0].ChunkID)
	}
	want := 2.0 / float64(rrfK+1)
	if math.Abs(out[0].Score-want) > 1e-9 {
		t.Errorf("shared chunk score: got %v want %v (=2/(k+1))", out[0].Score, want)
	}
	if out[1].Score >= out[0].Score {
		t.Errorf("non-shared chunk should rank below shared one")
	}
}

func TestFuseRRF_TruncatesToK(t *testing.T) {
	primary := []model.SearchHit{
		{ChunkID: 1}, {ChunkID: 2}, {ChunkID: 3},
	}
	secondary := []model.SearchHit{
		{ChunkID: 4}, {ChunkID: 5}, {ChunkID: 6},
	}
	out := fuseRRF(primary, secondary, 2)
	if len(out) != 2 {
		t.Fatalf("expected output truncated to k=2, got %d", len(out))
	}
}

func TestFuseRRF_ZeroKDefaults(t *testing.T) {
	primary := []model.SearchHit{{ChunkID: 1}}
	secondary := []model.SearchHit{{ChunkID: 2}}
	out := fuseRRF(primary, secondary, 0)
	if len(out) == 0 {
		t.Fatalf("expected non-empty output when k <= 0 (defaults to 10), got 0")
	}
}

// TestRerankFusionPoolSize pins the #519 rerank-pool fix: hybrid fusion keeps a
// candidate pool sized for the downstream rerank stage (not truncated to the
// final k), so the reranker can rescue a chunk fused below rank k.
func TestRerankFusionPoolSize(t *testing.T) {
	cases := []struct {
		name       string
		k          int
		rerankPool int
		want       int
	}{
		{"small k, default pool", 10, defaultRerankCandidatePool, defaultRerankCandidatePool},
		{"pool unset falls back to default", 10, 0, defaultRerankCandidatePool},
		{"k larger than pool is honored", 80, 50, 80},
		{"pool below hybrid floor lifts to floor", 5, 20, hybridCandidatePoolSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rerankFusionPoolSize(tc.k, tc.rerankPool); got != tc.want {
				t.Fatalf("rerankFusionPoolSize(%d, %d) = %d, want %d", tc.k, tc.rerankPool, got, tc.want)
			}
			// The pool must never be smaller than k, or rerank cannot return k hits.
			if got := rerankFusionPoolSize(tc.k, tc.rerankPool); got < tc.k {
				t.Fatalf("pool %d < k %d", got, tc.k)
			}
		})
	}
}

func scores(hits []model.SearchHit) []float64 {
	out := make([]float64, len(hits))
	for i, h := range hits {
		out[i] = h.Score
	}
	return out
}
