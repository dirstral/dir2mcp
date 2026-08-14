package retrieval

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// The refill loop accumulates its rounds (#818), so the merge step carries the
// pool's identity and ranking rules. These pin them directly.

// TestMergeRefillRounds_DedupsAndRanksByScore pins the two rules the merge owes
// the pipeline: one entry per chunk, and an order set by the authoritative Score
// rather than by the round that found the hit.
func TestMergeRefillRounds_DedupsAndRanksByScore(t *testing.T) {
	pool := []model.SearchHit{
		{ChunkID: 11, Score: 0.50},
		{ChunkID: 12, Score: 0.40},
	}
	round := []model.SearchHit{
		{ChunkID: 13, Score: 0.95},
		{ChunkID: 11, Score: 0.10}, // already in the pool
	}

	got := mergeRefillRounds(pool, round)

	wantIDs := []uint64{13, 11, 12}
	if len(got) != len(wantIDs) {
		t.Fatalf("merged pool has %d hits, want %d (%v)", len(got), len(wantIDs), hitChunkIDs(got))
	}
	for i, want := range wantIDs {
		if got[i].ChunkID != want {
			t.Fatalf("merged pool order = %v, want %v", hitChunkIDs(got), wantIDs)
		}
	}
	// First wins: chunk 11 keeps the score of the earlier, narrower round.
	if got[1].Score != 0.50 {
		t.Fatalf("chunk 11 scored %v after the merge, want the earlier round's 0.50", got[1].Score)
	}
}

// TestMergeRefillRounds_TiesKeepAccumulationOrder pins the tie-break: hits that
// score the same keep the order they were accumulated in, so the earlier round
// and each round's own rank order survive.
func TestMergeRefillRounds_TiesKeepAccumulationOrder(t *testing.T) {
	pool := []model.SearchHit{{ChunkID: 21, Score: 0.5}, {ChunkID: 22, Score: 0.5}}
	round := []model.SearchHit{{ChunkID: 23, Score: 0.5}, {ChunkID: 24, Score: 0.5}}

	got := hitChunkIDs(mergeRefillRounds(pool, round))
	want := []uint64{21, 22, 23, 24}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("merged pool order = %v, want %v", got, want)
		}
	}
}

// TestMergeRefillRounds_ZeroChunkIDIsNotDeduped pins that the merge reuses the
// pool's existing notion of chunk identity: a zero chunk id carries none, so two
// un-identified candidates must not collapse into one.
func TestMergeRefillRounds_ZeroChunkIDIsNotDeduped(t *testing.T) {
	pool := []model.SearchHit{{ChunkID: 0, RelPath: "a.md", Score: 0.9}}
	round := []model.SearchHit{{ChunkID: 0, RelPath: "b.md", Score: 0.8}}

	if got := mergeRefillRounds(pool, round); len(got) != 2 {
		t.Fatalf("merged pool has %d hits, want 2: a zero chunk id must not dedup", len(got))
	}
}

// hitChunkIDs projects the chunk ids of a pool, for readable failures.
func hitChunkIDs(hits []model.SearchHit) []uint64 {
	out := make([]uint64, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ChunkID)
	}
	return out
}
