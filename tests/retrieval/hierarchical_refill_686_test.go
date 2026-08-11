package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Hierarchical result loss: dir2mcp #686, a correctness follow-up to #329.
//
// A `summary` is a routing device: SPEC §9.7 says it never reaches the caller.
// The pipeline retrieved exactly the caller's `k` candidates and dropped the
// summaries afterwards, so every dropped summary silently stole one result slot
// from a real fine chunk. SPEC §9.7 also says that with the feature disabled
// retrieval behaves EXACTLY as flat retrieval, which under-returning breaks.
//
// Every test here indexes MORE eligible fine chunks than `k`, so a correct
// pipeline can always fill the caller's budget.

// summaryPoolCorpus wires a vector-only service over four ranked candidates:
//
//	chunk 10 → cosine 1.00, a `summary` of report.md (the best coarse match)
//	chunk 11 → cosine 0.80, a fine chunk of report.md
//	chunk 12 → cosine 0.60, a fine chunk of other.md
//	chunk 13 → cosine 0.40, a fine chunk of third.md
//
// With k=2 the caller must get two fine chunks: 11 and 12.
func summaryPoolCorpus(t *testing.T, enabled bool, st *summaryExpandStore) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 10, []float32{1, 0}, "docs/report.md", "md")
	addVecP(t, idx, 11, []float32{0.80, 0.60}, "docs/report.md", "md")
	addVecP(t, idx, 12, []float32{0.60, 0.80}, "docs/other.md", "md")
	addVecP(t, idx, 13, []float32{0.40, 0.92}, "docs/third.md", "md")

	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false) // vector-only: deterministic ranking, no BM25 dependency
	svc.SetChunkMetadata(10, model.SearchHit{
		ChunkID: 10, RelPath: "docs/report.md", DocType: "md",
		RepType: model.SummaryRepType, Snippet: "MODEL-GENERATED SUMMARY PROSE",
	})
	svc.SetChunkMetadata(11, model.SearchHit{
		ChunkID: 11, RelPath: "docs/report.md", DocType: "md",
		RepType: "raw_text", Snippet: "fine chunk one",
	})
	svc.SetChunkMetadata(12, model.SearchHit{
		ChunkID: 12, RelPath: "docs/other.md", DocType: "md",
		RepType: "raw_text", Snippet: "fine chunk two",
	})
	svc.SetChunkMetadata(13, model.SearchHit{
		ChunkID: 13, RelPath: "docs/third.md", DocType: "md",
		RepType: "raw_text", Snippet: "fine chunk three",
	})
	svc.SetHierarchical(enabled)
	return svc
}

// assertFullBudget fails when the caller asked for k and the corpus holds k
// eligible fine chunks, but fewer came back. The message reports the measured
// loss so a regression states its size.
func assertFullBudget(t *testing.T, hits []model.SearchHit, k int, what string) {
	t.Helper()
	assertNoSummaryHit(t, hits)
	if len(hits) < k {
		t.Fatalf("%s: asked for k=%d with %d eligible fine chunks indexed, got %d (ids %v): %d result slot(s) lost to dropped summary hits",
			what, k, k, len(hits), hitIDs(hits), k-len(hits))
	}
}

// TestSearch_Hierarchical_DisabledRefillsDroppedSummarySlots is the headline
// case of #686. The feature is OFF, so SPEC §9.7 requires the result to match a
// corpus that never had summary vectors: [11, 12]. Before the fix the summary
// took the first of the two retrieved slots and search returned only [11].
func TestSearch_Hierarchical_DisabledRefillsDroppedSummarySlots(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren()}
	svc := summaryPoolCorpus(t, false, st)

	hits := searchHits(t, svc, 2)
	assertFullBudget(t, hits, 2, "hierarchical disabled")
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
	if st.calls != 0 {
		t.Fatalf("expansion ran %d times with the feature disabled; it must be inert", st.calls)
	}
}

// TestSearch_Hierarchical_DisabledMatchesCorpusWithoutSummaries states the same
// requirement as an equivalence: the disabled result must equal the result of
// the identical corpus with the summary vector removed.
func TestSearch_Hierarchical_DisabledMatchesCorpusWithoutSummaries(t *testing.T) {
	withSummary := hitIDs(searchHits(t, summaryPoolCorpus(t, false, &summaryExpandStore{children: defaultChildren()}), 2))

	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 11, []float32{0.80, 0.60}, "docs/report.md", "md")
	addVecP(t, idx, 12, []float32{0.60, 0.80}, "docs/other.md", "md")
	addVecP(t, idx, 13, []float32{0.40, 0.92}, "docs/third.md", "md")
	flat := retrieval.NewService(&summaryExpandStore{}, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	flat.SetHybridEnabled(false)
	flat.SetChunkMetadata(11, model.SearchHit{ChunkID: 11, RelPath: "docs/report.md", DocType: "md", RepType: "raw_text", Snippet: "fine chunk one"})
	flat.SetChunkMetadata(12, model.SearchHit{ChunkID: 12, RelPath: "docs/other.md", DocType: "md", RepType: "raw_text", Snippet: "fine chunk two"})
	flat.SetChunkMetadata(13, model.SearchHit{ChunkID: 13, RelPath: "docs/third.md", DocType: "md", RepType: "raw_text", Snippet: "fine chunk three"})

	assertOrder(t, withSummary, hitIDs(searchHits(t, flat, 2)))
}

// TestSearch_Hierarchical_ExpansionErrorRefillsDroppedSlot pins the fail-open
// contract of SPEC §9.7 at the budget level: an expansion that errors drops its
// summary. The caller must still get the best available fine hits.
func TestSearch_Hierarchical_ExpansionErrorRefillsDroppedSlot(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren(), expandErr: errors.New("boom")}
	svc := summaryPoolCorpus(t, true, st)

	hits := searchHits(t, svc, 2)
	assertFullBudget(t, hits, 2, "expansion error")
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
}

// TestSearch_Hierarchical_FilteredChildrenRefillDroppedSlot pins the third drop
// path: expansion works, but the query filters remove every child, so the
// summary contributes nothing. The freed slot must go to the next fine chunk.
func TestSearch_Hierarchical_FilteredChildrenRefillDroppedSlot(t *testing.T) {
	st := &summaryExpandStore{children: map[uint64][]model.ChunkMetadata{
		// Every child sits under a path the caller excluded via path_prefix.
		10: {
			fineChunk(20, "archive/old.md", "out-of-scope child"),
			fineChunk(21, "archive/older.md", "out-of-scope child"),
		},
	}}
	svc := summaryPoolCorpus(t, true, st)

	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "quarterly results", K: 2, PathPrefix: "docs",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	assertFullBudget(t, hits, 2, "all children filtered out")
	for _, h := range hits {
		if h.RelPath == "archive/old.md" || h.RelPath == "archive/older.md" {
			t.Fatalf("expanded child %d bypassed the path_prefix filter", h.ChunkID)
		}
	}
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
	if st.calls == 0 {
		t.Fatal("expansion capability was never consulted")
	}
}

// TestSearch_Hierarchical_RefillStopsAtCorpusSize pins termination: when the
// corpus simply holds fewer fine chunks than k, the refill must stop instead of
// widening for ever, and it must return every fine chunk that does exist.
func TestSearch_Hierarchical_RefillStopsAtCorpusSize(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 10, []float32{1, 0}, "docs/report.md", "md")
	addVecP(t, idx, 11, []float32{0.80, 0.60}, "docs/report.md", "md")

	st := &summaryExpandStore{children: defaultChildren(), expandErr: errors.New("boom")}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false)
	svc.SetChunkMetadata(10, model.SearchHit{
		ChunkID: 10, RelPath: "docs/report.md", DocType: "md",
		RepType: model.SummaryRepType, Snippet: "MODEL-GENERATED SUMMARY PROSE",
	})
	svc.SetChunkMetadata(11, model.SearchHit{
		ChunkID: 11, RelPath: "docs/report.md", DocType: "md",
		RepType: "raw_text", Snippet: "fine chunk one",
	})
	svc.SetHierarchical(true)

	hits := searchHits(t, svc, 5)
	assertNoSummaryHit(t, hits)
	assertOrder(t, hitIDs(hits), []uint64{11})
}
