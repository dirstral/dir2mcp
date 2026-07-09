package retrieval

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// floorSurvivors runs applyMinScoreFloor for the given floor over hits and
// returns the surviving chunk IDs in order.
func floorSurvivors(floor float64, hits []model.SearchHit) []uint64 {
	svc := &Service{}
	svc.SetMinScore(floor)
	out := svc.applyMinScoreFloor(hits)
	ids := make([]uint64, 0, len(out))
	for _, h := range out {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

func eqIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestApplyMinScoreFloor_RRFScaleNotWipedOut is the #411 regression: an RRF
// (hybrid/HyDE/cross-lingual) result set has scores on the ~[0, 2/(rrfK+1)≈0.033]
// scale, so a cosine-shaped floor (0.3–0.5) applied to the RAW score would drop
// EVERY hit and return empty. With the normalized floor, a mid-range floor keeps
// the genuinely-relevant top hits and only drops the genuinely-weak tail.
func TestApplyMinScoreFloor_RRFScaleNotWipedOut(t *testing.T) {
	// Real RRF fusion output: three chunks fused from two ranked lists. All
	// scores are tiny (~0.02–0.033), well below any cosine-shaped floor.
	primary := []model.SearchHit{{ChunkID: 1}, {ChunkID: 2}, {ChunkID: 3}}
	secondary := []model.SearchHit{{ChunkID: 1}, {ChunkID: 2}, {ChunkID: 3}}
	fused := fuseRRF(primary, secondary, 10)
	if len(fused) != 3 {
		t.Fatalf("setup: expected 3 fused hits, got %d", len(fused))
	}
	// Sanity: confirm we really are on the RRF scale, not cosine.
	if fused[0].Score > 0.05 {
		t.Fatalf("setup: expected RRF-scale scores (<=0.05), top=%v", fused[0].Score)
	}

	// A cosine-shaped floor of 0.5 must NOT wipe the whole set (the pre-fix bug).
	// Chunk 1 is the strongest fused hit (normalized 1.0) and survives.
	got := floorSurvivors(0.5, fused)
	if len(got) == 0 {
		t.Fatalf("floor 0.5 wiped out all RRF hits (#411 regression); got empty")
	}
	if got[0] != fused[0].ChunkID {
		t.Fatalf("strongest fused hit must survive; want %d, got %v", fused[0].ChunkID, got)
	}

	// The floor still filters: floor 0 keeps everything; floor 1.0 keeps only the
	// top-normalized hit(s); the weakest hit (normalized 0.0) drops for any floor>0.
	if got := floorSurvivors(0, fused); len(got) != 3 {
		t.Fatalf("floor 0 must pass through all RRF hits; got %v", got)
	}
	weakest := fused[len(fused)-1].ChunkID
	for _, id := range floorSurvivors(0.01, fused) {
		if id == weakest {
			t.Fatalf("weakest normalized hit (chunk %d) must drop above floor 0; survived", weakest)
		}
	}
}

// TestApplyMinScoreFloor_Normalized pins the normalized RELATIVE semantics on a
// hand-built score set spanning both scales, independent of any retrieval mode.
func TestApplyMinScoreFloor_Normalized(t *testing.T) {
	// Scores {1.0, 0.5, 0.0} → min-max normalized {1.0, 0.5, 0.0} (denom 1.0, so
	// the normalized values are exact and boundary comparisons are fp-stable).
	hits := []model.SearchHit{
		{ChunkID: 1, Score: 1.0},
		{ChunkID: 2, Score: 0.5},
		{ChunkID: 3, Score: 0.0},
	}
	cases := []struct {
		floor float64
		want  []uint64
	}{
		{0, []uint64{1, 2, 3}},       // disabled: pass-through
		{0.4, []uint64{1, 2}},        // keeps top two (norm 1.0, 0.5), drops norm 0.0
		{0.5, []uint64{1, 2}},        // boundary: norm == floor is KEPT (strict <)
		{0.6, []uint64{1}},           // only the top survives
		{1.5, []uint64{}},            // above every normalized score → empty
	}
	for _, tc := range cases {
		got := floorSurvivors(tc.floor, hits)
		if !eqIDs(got, tc.want) {
			t.Fatalf("floor %v: want %v, got %v", tc.floor, tc.want, got)
		}
	}
}

// TestApplyMinScoreFloor_UniformSetNotWiped pins that an all-equal-score set
// (denominator 0) is never wiped by a floor — normalizedRelevance maps it to
// all-1, so every hit survives any floor <= 1. Without this guard a floor would
// drop every hit of a tied set.
func TestApplyMinScoreFloor_UniformSetNotWiped(t *testing.T) {
	hits := []model.SearchHit{
		{ChunkID: 1, Score: 0.03},
		{ChunkID: 2, Score: 0.03},
	}
	if got := floorSurvivors(0.9, hits); len(got) != 2 {
		t.Fatalf("uniform-score set must not be wiped by a floor; got %v", got)
	}
}
