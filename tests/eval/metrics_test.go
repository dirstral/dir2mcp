package tests

import (
	"math"
	"testing"
)

const eps = 1e-9

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= eps }

func TestRecallAtK(t *testing.T) {
	rel := relevantSet([]string{"a", "b", "c", "d"})
	cases := []struct {
		name      string
		retrieved []string
		k         int
		want      float64
	}{
		{"all-in-top-k", []string{"a", "b", "c", "d"}, 4, 1.0},
		{"half-in-top-k", []string{"a", "x", "b", "y"}, 4, 0.5},
		{"cutoff-limits", []string{"a", "b", "c", "d"}, 2, 0.5},
		{"none-relevant", []string{"x", "y", "z"}, 3, 0.0},
		{"k-exceeds-len", []string{"a", "b"}, 99, 0.5},
		{"dup-counted-once", []string{"a", "a", "b"}, 3, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RecallAtK(c.retrieved, rel, c.k)
			if !almostEqual(got, c.want) {
				t.Fatalf("RecallAtK=%v want %v", got, c.want)
			}
		})
	}
}

func TestRecallAtK_NoRelevantIsZero(t *testing.T) {
	if got := RecallAtK([]string{"a"}, map[string]struct{}{}, 1); got != 0 {
		t.Fatalf("empty relevant set must yield 0, got %v", got)
	}
}

func TestMRR(t *testing.T) {
	rel := relevantSet([]string{"a", "b"})
	cases := []struct {
		name      string
		retrieved []string
		want      float64
	}{
		{"first-position", []string{"a", "x", "y"}, 1.0},
		{"second-position", []string{"x", "a", "y"}, 0.5},
		{"third-position", []string{"x", "y", "b"}, 1.0 / 3.0},
		{"none-found", []string{"x", "y", "z"}, 0.0},
		{"first-relevant-wins", []string{"x", "b", "a"}, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MRR(c.retrieved, rel)
			if !almostEqual(got, c.want) {
				t.Fatalf("MRR=%v want %v", got, c.want)
			}
		})
	}
}

func TestNDCGAtK_PerfectRankingIsOne(t *testing.T) {
	rel := relevantSet([]string{"a", "b", "c"})
	got := NDCGAtK([]string{"a", "b", "c", "x"}, rel, 4)
	if !almostEqual(got, 1.0) {
		t.Fatalf("perfect ranking nDCG must be 1.0, got %v", got)
	}
}

func TestNDCGAtK_OrderMatters(t *testing.T) {
	rel := relevantSet([]string{"a"})
	// Single relevant item: rank 1 -> 1.0; rank 2 -> 1/log2(3); rank 3 -> 1/log2(4)=0.5.
	top := NDCGAtK([]string{"a", "x", "y"}, rel, 3)
	mid := NDCGAtK([]string{"x", "a", "y"}, rel, 3)
	low := NDCGAtK([]string{"x", "y", "a"}, rel, 3)
	if !(top > mid && mid > low) {
		t.Fatalf("nDCG must decrease as the relevant item sinks: top=%v mid=%v low=%v", top, mid, low)
	}
	if !almostEqual(top, 1.0) {
		t.Fatalf("rank-1 single relevant must be 1.0, got %v", top)
	}
	if !almostEqual(low, 0.5) {
		t.Fatalf("rank-3 single relevant must be 0.5, got %v", low)
	}
}

func TestNDCGAtK_IdealNormalizerCappedAtK(t *testing.T) {
	// 4 relevant items but k=2: a perfect top-2 must still score 1.0 because the
	// ideal DCG is computed over min(len(relevant), k) items.
	rel := relevantSet([]string{"a", "b", "c", "d"})
	got := NDCGAtK([]string{"a", "b", "x", "y"}, rel, 2)
	if !almostEqual(got, 1.0) {
		t.Fatalf("perfect top-k with more relevant than k must be 1.0, got %v", got)
	}
}

func TestNDCGAtK_NoRelevantIsZero(t *testing.T) {
	if got := NDCGAtK([]string{"a"}, map[string]struct{}{}, 1); got != 0 {
		t.Fatalf("empty relevant set must yield 0, got %v", got)
	}
}

func TestNDCGAtK_DuplicateNotDoubleCounted(t *testing.T) {
	rel := relevantSet([]string{"a"})
	// "a" appears twice; only the first credit should count, so this equals the
	// single-occurrence rank-1 score of 1.0.
	got := NDCGAtK([]string{"a", "a"}, rel, 2)
	if !almostEqual(got, 1.0) {
		t.Fatalf("duplicate relevant must not exceed 1.0, got %v", got)
	}
}

func TestMeanMetrics(t *testing.T) {
	got := MeanMetrics([]RankedMetrics{
		{RecallAtK: 1.0, NDCGAtK: 1.0, MRR: 1.0},
		{RecallAtK: 0.0, NDCGAtK: 0.0, MRR: 0.0},
	})
	if !almostEqual(got.RecallAtK, 0.5) || !almostEqual(got.NDCGAtK, 0.5) || !almostEqual(got.MRR, 0.5) {
		t.Fatalf("mean of {1,0} must be 0.5 across metrics, got %+v", got)
	}
	if zero := MeanMetrics(nil); zero != (RankedMetrics{}) {
		t.Fatalf("mean of empty must be zero value, got %+v", zero)
	}
}
