package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// timeCorpus builds a corpus of three time-spanned (media) hits plus one
// non-time (text) hit, so the §9.8 intra-document media time-window filter can be
// exercised against matchFilters. Distinct rel_paths sidestep media de-dup (the
// filter itself is path-agnostic); the time spans are the point:
//
//	id 1: [0,     10000]  (0–10s)
//	id 2: [20000, 30000]  (20–30s)
//	id 3: [50000, 60000]  (50–60s)
//	id 4: non-time (lines) — eligible only when the filter is inactive
func timeCorpus(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.97, 0.03})
	addVec(t, idx, 3, []float32{0.94, 0.06})
	addVec(t, idx, 4, []float32{0.91, 0.09})
	return newDateService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "a.mp4", DocType: "video", Snippet: "0-10s", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 10000}},
		2: {RelPath: "b.mp4", DocType: "video", Snippet: "20-30s", Span: model.Span{Kind: "time", StartMS: 20000, EndMS: 30000}},
		3: {RelPath: "c.mp4", DocType: "video", Snippet: "50-60s", Span: model.Span{Kind: "time", StartMS: 50000, EndMS: 60000}},
		4: {RelPath: "notes.txt", DocType: "text", Snippet: "text", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})
}

// TestSearch_TimeWindow_BothBoundsOverlap pins SPEC §9.8: a closed
// [time_from_ms, time_to_ms] window keeps only time-spanned hits whose span
// overlaps it, and excludes the non-time hit.
func TestSearch_TimeWindow_BothBoundsOverlap(t *testing.T) {
	svc := timeCorpus(t)
	// [15000, 35000] overlaps only the 20–30s segment (id 2).
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 15000,
		HasTimeTo: true, TimeToMS: 35000,
	})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("window [15s,35s] must yield only the 20–30s hit (id 2), got %v", got)
	}
}

// TestSearch_TimeWindow_OverlapNotContainment pins §9.8: a hit is kept when its
// span OVERLAPS the window, even if it is not contained — a boundary-straddling
// segment still surfaces.
func TestSearch_TimeWindow_OverlapNotContainment(t *testing.T) {
	svc := timeCorpus(t)
	// [8000, 22000] straddles the tail of id 1 (0–10s) and the head of id 2
	// (20–30s); neither is contained, but both overlap.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 8000,
		HasTimeTo: true, TimeToMS: 22000,
	})
	if len(got) != 2 || !hasID(got, 1) || !hasID(got, 2) {
		t.Fatalf("overlap (not containment) must keep ids 1 and 2, got %v", got)
	}
}

// TestSearch_TimeWindow_OpenLowerBound pins §9.8: an absent time_from_ms leaves
// the lower side open, so only time_to_ms constrains.
func TestSearch_TimeWindow_OpenLowerBound(t *testing.T) {
	svc := timeCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeTo: true, TimeToMS: 15000,
	})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("open-lower window (≤15s) must keep only the 0–10s hit (id 1), got %v", got)
	}
}

// TestSearch_TimeWindow_OpenUpperBound pins §9.8: an absent time_to_ms leaves the
// upper side open, so only time_from_ms constrains.
func TestSearch_TimeWindow_OpenUpperBound(t *testing.T) {
	svc := timeCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 35000,
	})
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("open-upper window (≥35s) must keep only the 50–60s hit (id 3), got %v", got)
	}
}

// TestSearch_TimeWindow_ZeroIsAValidLowerBound pins the crux of the presence
// design: time_from_ms=0 is an ACTIVE bound (video start), not "absent". A
// [0, 5000] window overlaps only the 0–10s hit and excludes the non-time hit,
// proving the filter is active even though the lower bound is the zero value.
func TestSearch_TimeWindow_ZeroIsAValidLowerBound(t *testing.T) {
	svc := timeCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 0,
		HasTimeTo: true, TimeToMS: 5000,
	})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("window [0s,5s] must be active and keep only the 0–10s hit (id 1), got %v", got)
	}
}

// TestSearch_TimeWindow_NonTimeHitExcludedWhenActive pins §9.8: when either bound
// is present, a hit with any non-time span never matches.
func TestSearch_TimeWindow_NonTimeHitExcludedWhenActive(t *testing.T) {
	svc := timeCorpus(t)
	// A wide window covering every media span; the text hit (id 4) must still be
	// excluded because it carries no time span.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 0,
		HasTimeTo: true, TimeToMS: 60000,
	})
	if hasID(got, 4) {
		t.Fatalf("a non-time hit must never match an active time window, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("the wide window must keep all three media hits, got %v", got)
	}
}

// TestSearch_TimeWindow_BothAbsentIsNoFilter pins §9.8: with both bounds absent
// the filter is a no-op — every candidate is returned, including the non-time hit.
func TestSearch_TimeWindow_BothAbsentIsNoFilter(t *testing.T) {
	svc := timeCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10})
	if len(got) != 4 {
		t.Fatalf("no time window must return all four hits (non-time included), got %d: %v", len(got), got)
	}
}

// TestSearch_TimeWindow_NoMatchIsEmptyNotError pins §9.8: a valid window matching
// nothing returns an empty hit list, not an error.
func TestSearch_TimeWindow_NoMatchIsEmptyNotError(t *testing.T) {
	svc := timeCorpus(t)
	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "x", K: 10,
		HasTimeFrom: true, TimeFromMS: 70000,
		HasTimeTo: true, TimeToMS: 80000,
	})
	if err != nil {
		t.Fatalf("a no-match time window must not error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a no-match time window must return an empty hit list, got %d", len(hits))
	}
}

// TestSearch_TimeWindow_KCountsOnlyInWindow pins §9.8: the window is applied at
// candidate selection, so k counts only in-window hits — id 1 has the highest
// score but is out of the window, yet k=1 still surfaces the in-window id 2.
func TestSearch_TimeWindow_KCountsOnlyInWindow(t *testing.T) {
	svc := timeCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query: "x", K: 1,
		HasTimeFrom: true, TimeFromMS: 15000,
		HasTimeTo: true, TimeToMS: 35000,
	})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("k=1 over the [15s,35s] window must return the in-window id 2, got %v", got)
	}
}
