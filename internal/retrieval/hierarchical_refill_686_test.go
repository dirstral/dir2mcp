package retrieval

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// The refill loop of #686 re-queries the index, so its stop rule must be exact:
// it has to keep going while dropped summary hits still cost the caller result
// slots, and it has to stop on every other shape. This table pins each stop.
func TestNeedSummaryRefill(t *testing.T) {
	cases := []struct {
		name       string
		got        int
		poolK      int
		summaries  int
		rawLen     int
		fetchK     int
		round      int
		wantRefill bool
	}{
		{"flat pool with no summary", 5, 10, 0, 10, 10, 0, false},
		{"budget already met", 10, 10, 2, 10, 10, 0, false},
		{"summary took a slot", 1, 2, 1, 2, 2, 0, true},
		{"index exhausted", 1, 2, 1, 2, 3, 1, false},
		{"round cap reached", 1, 2, 1, 3, 3, 2, false},
		{"widened pool would not grow", 1, 2, 1, 3, 3, 1, false},
		{"pool already at the cap", 1, 300, 5, summaryRefillMaxPool, summaryRefillMaxPool, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := needSummaryRefill(tc.got, tc.poolK, tc.summaries, tc.rawLen, tc.fetchK, tc.round)
			if got != tc.wantRefill {
				t.Fatalf("needSummaryRefill(got=%d poolK=%d summaries=%d rawLen=%d fetchK=%d round=%d) = %v, want %v",
					tc.got, tc.poolK, tc.summaries, tc.rawLen, tc.fetchK, tc.round, got, tc.wantRefill)
			}
		})
	}
}

// countSummaryHits is the number of result slots a pool loses to routing nodes,
// so it must count every summary and only summaries.
func TestCountSummaryHits(t *testing.T) {
	pool := []model.SearchHit{
		{ChunkID: 1, RepType: model.SummaryRepType},
		{ChunkID: 2, RepType: "raw_text"},
		{ChunkID: 3, RepType: model.SummaryRepType},
		{ChunkID: 4, RepType: "transcript"},
	}
	if n := countSummaryHits(pool); n != 2 {
		t.Fatalf("countSummaryHits = %d, want 2", n)
	}
	if n := countSummaryHits(nil); n != 0 {
		t.Fatalf("countSummaryHits(nil) = %d, want 0", n)
	}
}
