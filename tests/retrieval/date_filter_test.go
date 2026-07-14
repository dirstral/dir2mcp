package tests

import (
	"context"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// unixDay returns the Unix seconds at 00:00:00Z of the given UTC calendar date,
// the calendar anchor a document with that mtime would carry (SPEC §9.6).
func unixDay(year int, month time.Month, day int) int64 {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix()
}

// newDateService builds a vector-only retrieval service over idx and registers
// in-memory chunk metadata carrying each hit's MTimeUnix, mirroring the
// production warm/on-index path so matchFilters sees the document-date anchor
// (SPEC §9.6). It reuses addVec/fakeRetrievalEmbedder from the language tests.
func newDateService(t *testing.T, idx *index.HNSWIndex, meta map[uint64]model.SearchHit) *retrieval.Service {
	t.Helper()
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for id, hit := range meta {
		svc.SetChunkMetadata(id, hit)
	}
	return svc
}

// dateCorpus builds a three-document corpus with mtimes on 2026-03-15,
// 2026-04-15, and 2026-05-15 plus one document with an unknown (0) mtime.
func dateCorpus(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.97, 0.03})
	addVec(t, idx, 3, []float32{0.94, 0.06})
	addVec(t, idx, 4, []float32{0.91, 0.09})
	return newDateService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "mar.txt", DocType: "text", Snippet: "march", MTimeUnix: unixDay(2026, 3, 15), Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "apr.txt", DocType: "text", Snippet: "april", MTimeUnix: unixDay(2026, 4, 15), Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		3: {RelPath: "may.txt", DocType: "text", Snippet: "may", MTimeUnix: unixDay(2026, 5, 15), Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		4: {RelPath: "unknown.txt", DocType: "text", Snippet: "undated", MTimeUnix: 0, Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})
}

func dateSearchIDs(t *testing.T, svc *retrieval.Service, q model.SearchQuery) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

func hasID(ids []uint64, want uint64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSearch_DateFilter_BothBoundsRestrictsToWindow pins SPEC §9.6: a closed
// [date_from, date_to] window keeps only documents whose mtime is inside it,
// inclusively, and excludes those outside and the unknown-mtime document.
func TestSearch_DateFilter_BothBoundsRestrictsToWindow(t *testing.T) {
	svc := dateCorpus(t)
	// [2026-04-01, 2026-04-30] contains only the apr.txt doc (2026-04-15).
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query:    "x",
		K:        10,
		DateFrom: unixDay(2026, 4, 1),
		DateTo:   time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC).Unix(),
	})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("window [Apr 1, Apr 30] must yield only apr.txt (id 2), got %v", got)
	}
}

// TestSearch_DateFilter_InclusiveBounds pins §9.6 inclusivity: a document whose
// mtime falls exactly on either bound is kept.
func TestSearch_DateFilter_InclusiveBounds(t *testing.T) {
	svc := dateCorpus(t)
	// Bounds land exactly on the mar (from) and may (to) anchors.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query:    "x",
		K:        10,
		DateFrom: unixDay(2026, 3, 15),
		DateTo:   unixDay(2026, 5, 15),
	})
	if len(got) != 3 {
		t.Fatalf("inclusive bounds on the mar/may anchors must keep all three dated docs, got %d: %v", len(got), got)
	}
	if hasID(got, 4) {
		t.Fatalf("unknown-mtime doc must never match a bounded window, got %v", got)
	}
}

// TestSearch_DateFilter_OpenLowerBound pins §9.6: an absent date_from leaves the
// lower side open, so only date_to constrains.
func TestSearch_DateFilter_OpenLowerBound(t *testing.T) {
	svc := dateCorpus(t)
	// Only date_to = end of April: mar and apr survive, may is excluded.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query:  "x",
		K:      10,
		DateTo: time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC).Unix(),
	})
	if len(got) != 2 || !hasID(got, 1) || !hasID(got, 2) {
		t.Fatalf("open-lower window (≤ Apr 30) must keep mar+apr only, got %v", got)
	}
}

// TestSearch_DateFilter_OpenUpperBound pins §9.6: an absent date_to leaves the
// upper side open, so only date_from constrains.
func TestSearch_DateFilter_OpenUpperBound(t *testing.T) {
	svc := dateCorpus(t)
	// Only date_from = start of April: apr and may survive, mar is excluded.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query:    "x",
		K:        10,
		DateFrom: unixDay(2026, 4, 1),
	})
	if len(got) != 2 || !hasID(got, 2) || !hasID(got, 3) {
		t.Fatalf("open-upper window (≥ Apr 1) must keep apr+may only, got %v", got)
	}
}

// TestSearch_DateFilter_BothOpenIsNoFilter pins §9.6: with both bounds absent the
// filter is a no-op — every candidate is returned, including the unknown-mtime doc.
func TestSearch_DateFilter_BothOpenIsNoFilter(t *testing.T) {
	svc := dateCorpus(t)
	got := dateSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10})
	if len(got) != 4 {
		t.Fatalf("no date window must return all four docs (unknown mtime included), got %d: %v", len(got), got)
	}
}

// TestSearch_DateFilter_UnknownMtimeExcludedWhenBoundSet pins §9.6: a document
// with an unknown (0) mtime never matches a window that sets either bound, but is
// returned unchanged when no bound is set (mirrors the unknown-language rule).
func TestSearch_DateFilter_UnknownMtimeExcludedWhenBoundSet(t *testing.T) {
	svc := dateCorpus(t)

	// A lower bound alone still excludes the unknown-mtime doc.
	lower := dateSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, DateFrom: unixDay(2026, 1, 1)})
	if hasID(lower, 4) {
		t.Fatalf("unknown-mtime doc must be excluded when date_from is set, got %v", lower)
	}
	// An upper bound alone likewise excludes it.
	upper := dateSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, DateTo: unixDay(2026, 12, 31)})
	if hasID(upper, 4) {
		t.Fatalf("unknown-mtime doc must be excluded when date_to is set, got %v", upper)
	}
}

// TestSearch_DateFilter_NoMatchIsEmptyNotError pins §9.6: a valid window that
// matches nothing in the corpus returns an empty hit list, not an error.
func TestSearch_DateFilter_NoMatchIsEmptyNotError(t *testing.T) {
	svc := dateCorpus(t)
	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query:    "x",
		K:        10,
		DateFrom: unixDay(2027, 1, 1),
		DateTo:   unixDay(2027, 12, 31),
	})
	if err != nil {
		t.Fatalf("a no-match date window must not error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a no-match date window must return an empty hit list, got %d", len(hits))
	}
}

// TestSearch_DateFilter_KCountsOnlyInWindow pins §9.6: the window is applied at
// candidate selection, so k counts only in-window hits — a k=1 request over a
// window that admits a single doc returns that doc, even when a nearer-by-score
// out-of-window doc exists.
func TestSearch_DateFilter_KCountsOnlyInWindow(t *testing.T) {
	svc := dateCorpus(t)
	// mar.txt has the highest score (vector {1,0}) but is out of the April window;
	// k=1 must still surface apr.txt rather than returning the out-of-window mar.
	got := dateSearchIDs(t, svc, model.SearchQuery{
		Query:    "x",
		K:        1,
		DateFrom: unixDay(2026, 4, 1),
		DateTo:   time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC).Unix(),
	})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("k=1 over the April window must return apr.txt (id 2), got %v", got)
	}
}
