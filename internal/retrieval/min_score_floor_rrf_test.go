package retrieval

import (
	"math"
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
	// Chunk 1 is the strongest fused hit (ratio 1.0) and survives.
	got := floorSurvivors(0.5, fused)
	if len(got) == 0 {
		t.Fatalf("floor 0.5 wiped out all RRF hits (#411 regression); got empty")
	}
	if got[0] != fused[0].ChunkID {
		t.Fatalf("strongest fused hit must survive; want %d, got %v", fused[0].ChunkID, got)
	}

	// These three fused scores are near-identical (2/61, 2/62, 2/63), so every
	// hit is within 4% of the best and NONE of them is weak relative to it. The
	// whole set must survive the shipped default floor (#858). Before that fix
	// the min-max normalization mapped chunk 3 to exactly 0.0 and deleted it on
	// every query, because it was the lowest and not because it was weak.
	if got := floorSurvivors(0.05, fused); len(got) != 3 {
		t.Fatalf("near-identical RRF hits must all survive the default floor (#858); got %v", got)
	}
	if got := floorSurvivors(0, fused); len(got) != 3 {
		t.Fatalf("floor 0 must pass through all RRF hits; got %v", got)
	}

	// The floor still discriminates ON the RRF scale rather than becoming a
	// no-op: a floor above the weaker hits' ratio to the best prunes them.
	if got := floorSurvivors(0.99, fused); !eqIDs(got, []uint64{fused[0].ChunkID}) {
		t.Fatalf("floor 0.99 must keep only the best RRF hit; got %v", got)
	}
}

// TestApplyMinScoreFloor_Normalized pins the RELATIVE ratio semantics on a
// hand-built score set spanning both scales, independent of any retrieval mode.
func TestApplyMinScoreFloor_Normalized(t *testing.T) {
	// Scores {1.0, 0.5, 0.0} → ratios to the best {1.0, 0.5, 0.0} (best 1.0, so
	// the ratios are exact and boundary comparisons are fp-stable). Chunk 3 is
	// dropped by a positive floor because its score really is 0, not because it
	// is the lowest of the set (#858).
	hits := []model.SearchHit{
		{ChunkID: 1, Score: 1.0},
		{ChunkID: 2, Score: 0.5},
		{ChunkID: 3, Score: 0.0},
	}
	cases := []struct {
		floor float64
		want  []uint64
	}{
		{0, []uint64{1, 2, 3}}, // disabled: pass-through
		{0.4, []uint64{1, 2}},  // keeps top two (ratio 1.0, 0.5), drops ratio 0.0
		{0.5, []uint64{1, 2}},  // boundary: ratio == floor is KEPT (strict <)
		{0.6, []uint64{1}},     // only the top survives
		{1.5, []uint64{}},      // above every ratio (max is 1.0) → empty
	}
	for _, tc := range cases {
		got := floorSurvivors(tc.floor, hits)
		if !eqIDs(got, tc.want) {
			t.Fatalf("floor %v: want %v, got %v", tc.floor, tc.want, got)
		}
	}
}

// TestApplyMinScoreFloor_UniformSetNotWiped pins that an all-equal-score set is
// never wiped by a floor — relativeToBest maps every member to 1.0, so each one
// survives any floor <= 1. Without this a floor would drop every hit of a tied
// set.
func TestApplyMinScoreFloor_UniformSetNotWiped(t *testing.T) {
	hits := []model.SearchHit{
		{ChunkID: 1, Score: 0.03},
		{ChunkID: 2, Score: 0.03},
	}
	if got := floorSurvivors(0.9, hits); len(got) != 2 {
		t.Fatalf("uniform-score set must not be wiped by a floor; got %v", got)
	}
}

// TestApplyMinScoreFloor_NoPositiveBestIsPassThrough pins the fail-open rule for
// a pool where no ratio is defined (#858): the best score is <= 0, or is not
// finite, so score/best would either divide by zero, invert the ordering (all
// scores negative), or degenerate (+Inf). The floor keeps such a set intact
// rather than pruning on a comparison that carries no meaning.
func TestApplyMinScoreFloor_NoPositiveBestIsPassThrough(t *testing.T) {
	cases := []struct {
		name string
		hits []model.SearchHit
	}{
		{"all zero", []model.SearchHit{{ChunkID: 1, Score: 0}, {ChunkID: 2, Score: 0}}},
		{"all negative", []model.SearchHit{{ChunkID: 1, Score: -0.1}, {ChunkID: 2, Score: -0.9}}},
		{"infinite best", []model.SearchHit{{ChunkID: 1, Score: math.Inf(1)}, {ChunkID: 2, Score: 0.5}}},
	}
	for _, tc := range cases {
		if got := floorSurvivors(0.5, tc.hits); len(got) != len(tc.hits) {
			t.Fatalf("%s: floor must fail open when no ratio is defined; got %v", tc.name, got)
		}
	}
}

// TestApplyMinScoreFloor_NegativeScoreDropsBelowPositiveBest pins the other
// half of that rule: when the best score IS positive, an anti-correlated
// (negative) hit has a negative ratio and is pruned, which is a judgement about
// its own score and not about its rank in the set.
func TestApplyMinScoreFloor_NegativeScoreDropsBelowPositiveBest(t *testing.T) {
	hits := []model.SearchHit{
		{ChunkID: 1, Score: 0.8},
		{ChunkID: 2, Score: 0.79},
		{ChunkID: 3, Score: -0.2},
	}
	if got := floorSurvivors(0.05, hits); !eqIDs(got, []uint64{1, 2}) {
		t.Fatalf("negative-score hit must drop, near-equal hits must stay; got %v", got)
	}
}
