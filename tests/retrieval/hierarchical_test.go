package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Hierarchical (coarse-to-fine) retrieval — SPEC §9.7, dir2mcp #329.
//
// The invariant under test throughout: SUMMARIES RETRIEVE, CHUNKS CITE. A
// `summary` hit is a routing device that expands to the fine chunks its coverage
// names; it must never itself reach the caller, because a model-generated
// summary is not source text and would break citation faithfulness (#403).

// summaryExpandStore is a minimal model.Store that also implements the
// coarse-to-fine expansion capability: it maps a summary chunk id to the fine
// chunks beneath it. expandErr, when set, makes every expansion fail so the
// fail-open path can be exercised.
type summaryExpandStore struct {
	children  map[uint64][]model.ChunkMetadata
	expandErr error
	calls     int
}

func (s *summaryExpandStore) ExpandSummaryChunk(_ context.Context, chunkID uint64) ([]model.ChunkMetadata, error) {
	s.calls++
	if s.expandErr != nil {
		return nil, s.expandErr
	}
	return s.children[chunkID], nil
}

func (s *summaryExpandStore) Init(context.Context) error                           { return nil }
func (s *summaryExpandStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *summaryExpandStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *summaryExpandStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *summaryExpandStore) Close() error { return nil }

func fineChunk(id uint64, relPath, snippet string) model.ChunkMetadata {
	return model.ChunkMetadata{
		ChunkID: id,
		RelPath: relPath,
		DocType: "md",
		RepType: "raw_text",
		Snippet: snippet,
	}
}

// newHierarchicalService wires a vector-only service over three indexed
// candidates:
//
//	chunk 10 → cosine 1.00, a `summary` of report.md (the best coarse match)
//	chunk 11 → cosine 0.80, a fine chunk of report.md
//	chunk 12 → cosine 0.60, a fine chunk of other.md
//
// The summary covers fine chunks {11, 20, 21} of its own document, so 11 is
// reachable BOTH directly and via the summary — the dedup case.
func newHierarchicalService(t *testing.T, enabled bool, st *summaryExpandStore) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 10, []float32{1, 0})
	addVec(t, idx, 11, []float32{0.80, 0.60})
	addVec(t, idx, 12, []float32{0.60, 0.80})

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
		RepType: "raw_text", Snippet: "unrelated fine chunk",
	})
	svc.SetHierarchical(enabled)
	return svc
}

func defaultChildren() map[uint64][]model.ChunkMetadata {
	return map[uint64][]model.ChunkMetadata{
		10: {
			fineChunk(11, "docs/report.md", "fine chunk one"),
			fineChunk(20, "docs/report.md", "fine chunk two"),
			fineChunk(21, "docs/report.md", "fine chunk three"),
		},
	}
}

func searchHits(t *testing.T, svc *retrieval.Service, k int) []model.SearchHit {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "quarterly results", K: k})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func assertNoSummaryHit(t *testing.T, hits []model.SearchHit) {
	t.Helper()
	for _, h := range hits {
		if model.IsSummaryRepType(h.RepType) {
			t.Fatalf("citation-faithfulness violation: a %q hit reached the caller (chunk %d, snippet %q)",
				h.RepType, h.ChunkID, h.Snippet)
		}
	}
}

// TestSearch_Hierarchical_ExpandsSummaryToFineChunks pins the core §9.7 flow:
// the top-ranked hit is a summary, and what comes back is the FINE chunks it
// covers — never the summary itself.
func TestSearch_Hierarchical_ExpandsSummaryToFineChunks(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren()}
	svc := newHierarchicalService(t, true, st)

	hits := searchHits(t, svc, 10)
	assertNoSummaryHit(t, hits)

	got := hitIDs(hits)
	// The summary's children replace it in place (keeping its rank), then the
	// directly-retrieved fine chunks follow.
	assertOrder(t, got, []uint64{11, 20, 21, 12})
	if st.calls == 0 {
		t.Fatal("expansion capability was never consulted")
	}
}

// TestSearch_Hierarchical_DedupsChunkReachedTwice pins §9.7 step 3: a fine chunk
// reached BOTH directly and through its summary appears exactly once.
func TestSearch_Hierarchical_DedupsChunkReachedTwice(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren()}
	svc := newHierarchicalService(t, true, st)

	hits := searchHits(t, svc, 10)
	seen := map[uint64]int{}
	for _, h := range hits {
		seen[h.ChunkID]++
	}
	if seen[11] != 1 {
		t.Fatalf("chunk 11 is reachable directly AND via the summary; want 1 occurrence, got %d (ids %v)",
			seen[11], hitIDs(hits))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("chunk %d appears %d times; the merged pool must be deduped (ids %v)", id, n, hitIDs(hits))
		}
	}
}

// TestSearch_Hierarchical_DisabledDropsSummaryHits pins the unconditional half
// of the invariant: with the feature OFF a summary is not expanded — but it is
// still never returned, so a stale coarse vector can never become a citation.
func TestSearch_Hierarchical_DisabledDropsSummaryHits(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren()}
	svc := newHierarchicalService(t, false, st)

	hits := searchHits(t, svc, 10)
	assertNoSummaryHit(t, hits)
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
	if st.calls != 0 {
		t.Fatalf("expansion ran %d times with the feature disabled; it must be inert", st.calls)
	}
}

// TestSearch_Hierarchical_ExpansionFailureIsFailOpen pins the §9.7 fail-open
// contract on the query side: when expansion errors the summary is dropped and
// the flat hits are still returned — the query never fails.
func TestSearch_Hierarchical_ExpansionFailureIsFailOpen(t *testing.T) {
	st := &summaryExpandStore{children: defaultChildren(), expandErr: errors.New("boom")}
	svc := newHierarchicalService(t, true, st)

	hits := searchHits(t, svc, 10)
	assertNoSummaryHit(t, hits)
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
}

// TestSearch_Hierarchical_AppliesQueryFiltersToExpandedChildren pins that
// expansion cannot smuggle a filtered-out chunk into the result: children are
// reached by coverage identity, so the query's candidate filters must gate them
// exactly as they gate directly-retrieved hits.
//
// The index payloads carry rel_path here so the FilteringIndex also evaluates
// the pushed-down predicate on the DIRECTLY retrieved candidates, matching how a
// real backend narrows the pool.
func TestSearch_Hierarchical_AppliesQueryFiltersToExpandedChildren(t *testing.T) {
	st := &summaryExpandStore{children: map[uint64][]model.ChunkMetadata{
		10: {
			fineChunk(20, "docs/report.md", "in-scope child"),
			// A child under a path the caller excluded via path_prefix.
			fineChunk(21, "archive/old.md", "out-of-scope child"),
		},
	}}
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 10, []float32{1, 0}, "docs/report.md", "md")
	addVecP(t, idx, 11, []float32{0.80, 0.60}, "docs/report.md", "md")

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

	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "quarterly results", K: 10, PathPrefix: "docs",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	assertNoSummaryHit(t, hits)
	for _, h := range hits {
		if h.RelPath == "archive/old.md" {
			t.Fatalf("expanded child %d bypassed the path_prefix filter", h.ChunkID)
		}
	}
	assertOrder(t, hitIDs(hits), []uint64{20, 11})
}

// TestSearch_Hierarchical_NoSummariesIsFlatRetrieval pins the default: a corpus
// with no summary representations behaves exactly as flat retrieval and never
// touches the expansion capability, whether the feature is on or off.
func TestSearch_Hierarchical_NoSummariesIsFlatRetrieval(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		st := &summaryExpandStore{children: defaultChildren()}
		idx := index.NewHNSWIndex("")
		addVec(t, idx, 1, []float32{1, 0})
		addVec(t, idx, 2, []float32{0.80, 0.60})

		svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
			"mistral-embed": {1, 0},
		}}, nil)
		svc.SetHybridEnabled(false)
		svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", RepType: "raw_text", Snippet: "alpha"})
		svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", RepType: "raw_text", Snippet: "beta"})
		svc.SetHierarchical(enabled)

		assertOrder(t, hitIDs(searchHits(t, svc, 10)), []uint64{1, 2})
		if st.calls != 0 {
			t.Fatalf("enabled=%v: expansion ran on a corpus with no summaries", enabled)
		}
	}
}
