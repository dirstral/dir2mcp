package tests

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// newCrossFileDedupService builds a vector-only retrieval service over the
// given index (nil store keeps hybrid search from engaging) wired with the
// SPEC 9.2 cross-file dedup toggle and rel_path → content_hash map.
func newCrossFileDedupService(idx *index.HNSWIndex, enabled bool, hashes []model.DocumentHash) *retrieval.Service {
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetCrossFileDedupEnabled(enabled)
	svc.SetDocumentHashes(hashes)
	return svc
}

func searchChunkIDs(t *testing.T, svc *retrieval.Service, query string) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: query, K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

// TestSearch_CrossFileDedup_CollapsesAliasesKeepsBestRanked pins SPEC 9.2: two
// alias documents (same content_hash, different rel_path) collapse to a single
// survivor, the best-ranked one is kept, and the relative order of the
// remaining distinct hit is preserved.
func TestSearch_CrossFileDedup_CollapsesAliasesKeepsBestRanked(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// chunk 1 (a.md) ranks best, chunk 2 (copy/a.md) is its byte-identical
	// alias and ranks slightly lower, chunk 3 (b.md) is distinct.
	addVec(t, idx, 1, []float32{1, 0})       // a.md       (hash H1) - best
	addVec(t, idx, 2, []float32{0.99, 0.01}) // copy/a.md  (hash H1) - alias
	addVec(t, idx, 3, []float32{0.95, 0.05}) // b.md       (hash H2) - distinct

	svc := newCrossFileDedupService(idx, true, []model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
		{RelPath: "b.md", ContentHash: "H2"},
	})
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "copy/a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "b.md", Snippet: "beta"})

	got := searchChunkIDs(t, svc, "alpha")

	// Expect [1, 3]: chunk 2 (alias of chunk 1) collapsed; best-ranked alias
	// (chunk 1) kept; distinct chunk 3 preserved after it.
	want := []uint64{1, 3}
	if len(got) != len(want) {
		t.Fatalf("expected %v survivors, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/survivor mismatch: want %v, got %v", want, got)
		}
	}
}

// TestSearch_CrossFileDedup_DisabledIsPassThrough pins SPEC 9.2 default-off:
// with dedup.retrieval=false the pre-dedup candidate set is returned unchanged,
// even though two candidates share a content_hash.
func TestSearch_CrossFileDedup_DisabledIsPassThrough(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.99, 0.01})

	svc := newCrossFileDedupService(idx, false, []model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "copy/a.md", Snippet: "alpha"})

	got := searchChunkIDs(t, svc, "alpha")
	if len(got) != 2 {
		t.Fatalf("disabled dedup must pass through both candidates; got %v", got)
	}
}

// TestSearch_CrossFileDedup_EmptyContentHashNotCollapsed pins SPEC 9.2: an
// empty/absent content_hash is NEVER grouped — two such candidates both survive
// even with dedup enabled.
func TestSearch_CrossFileDedup_EmptyContentHashNotCollapsed(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.99, 0.01})

	// Both rel_paths map to an empty content_hash (SetDocumentHashes drops
	// these), so neither is grouped.
	svc := newCrossFileDedupService(idx, true, []model.DocumentHash{
		{RelPath: "a.md", ContentHash: ""},
		{RelPath: "b.md", ContentHash: ""},
	})
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "b.md", Snippet: "alpha"})

	got := searchChunkIDs(t, svc, "alpha")
	if len(got) != 2 {
		t.Fatalf("empty content_hash must never collapse; got %v", got)
	}
}

// TestSearch_CrossFileDedup_NoMapIsPassThrough pins the pass-through path when
// the store yielded no hash map (enabled but empty): search is unchanged.
func TestSearch_CrossFileDedup_NoMapIsPassThrough(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.99, 0.01})

	svc := newCrossFileDedupService(idx, true, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "copy/a.md", Snippet: "alpha"})

	got := searchChunkIDs(t, svc, "alpha")
	if len(got) != 2 {
		t.Fatalf("no hash map must pass through; got %v", got)
	}
}

// TestSearch_CrossFileDedup_SingleDocumentKeepsAllItsHits pins #782: cross-file
// dedup groups across DISTINCT documents only. Every hit from one document maps
// to that document's own content_hash, so grouping on the hash alone collapsed a
// single-file corpus (the normal shape of a video corpus, whose retrievable
// units are intra-document time spans) to exactly one hit.
func TestSearch_CrossFileDedup_SingleDocumentKeepsAllItsHits(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.99, 0.01})
	addVec(t, idx, 3, []float32{0.95, 0.05})

	svc := newCrossFileDedupService(idx, true, []model.DocumentHash{
		{RelPath: "game.mp4", ContentHash: "H1"},
	})
	// Three distinct moments in the same video: none is a duplicate of another.
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "game.mp4", Snippet: "struck out swinging",
		Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "game.mp4", Snippet: "walked",
		Span: model.Span{Kind: "time", StartMS: 3000, EndMS: 4000}})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "game.mp4", Snippet: "grand slam",
		Span: model.Span{Kind: "time", StartMS: 5000, EndMS: 6000}})

	got := searchChunkIDs(t, svc, "at bat")

	want := []uint64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("a document must never dedup against itself: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/survivor mismatch: want %v, got %v", want, got)
		}
	}
}

// TestSearch_CrossFileDedup_AliasCollapsedButOriginalHitsSurvive pins the two
// rules together: the alias document collapses away (SPEC 9.2) while every hit
// from the surviving document is kept, including ones ranked below the alias.
func TestSearch_CrossFileDedup_AliasCollapsedButOriginalHitsSurvive(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})       // a.md      (hash H1) - best
	addVec(t, idx, 2, []float32{0.99, 0.01}) // copy/a.md (hash H1) - alias
	addVec(t, idx, 3, []float32{0.95, 0.05}) // a.md      (hash H1) - second chunk

	svc := newCrossFileDedupService(idx, true, []model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "copy/a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "a.md", Snippet: "alpha again"})

	var logbuf bytes.Buffer
	svc.SetLogger(log.New(&logbuf, "", 0))

	got := searchChunkIDs(t, svc, "alpha")

	want := []uint64{1, 3}
	if len(got) != len(want) {
		t.Fatalf("want %v (alias 2 collapsed, both a.md chunks kept), got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/survivor mismatch: want %v, got %v", want, got)
		}
	}
	if !strings.Contains(logbuf.String(), "dedup: collapsed 1 of 3") {
		t.Fatalf("collapsed candidates must be reported; log was %q", logbuf.String())
	}
}

// TestSearch_CrossFileDedup_IndexBothMergedPool pins SPEC 9.2 for index=both:
// a byte-identical duplicate that surfaces once in the text pool and once in the
// code pool (as distinct chunks) must collapse to a single survivor in the
// merged pool, not just within each axis.
func TestSearch_CrossFileDedup_IndexBothMergedPool(t *testing.T) {
	textIdx := index.NewHNSWIndex("")
	codeIdx := index.NewHNSWIndex("")
	addVec(t, textIdx, 1, []float32{1, 0})       // a.md      (hash H1) in text axis
	addVec(t, codeIdx, 2, []float32{0.99, 0.01}) // copy/a.md (hash H1) in code axis

	svc := retrieval.NewService(nil, textIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {1, 0},
	}}, nil)
	svc.SetCodeIndex(codeIdx)
	svc.SetCrossFileDedupEnabled(true)
	svc.SetDocumentHashes([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "copy/a.md", Snippet: "alpha"})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10, Index: "both"})
	if err != nil {
		t.Fatalf("Search(index=both): %v", err)
	}
	if len(hits) != 1 {
		ids := make([]uint64, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ChunkID)
		}
		t.Fatalf("index=both cross-file dedup: expected 1 survivor, got %v", ids)
	}
	if hits[0].ChunkID != 1 {
		t.Fatalf("expected best-ranked survivor chunk 1, got %d", hits[0].ChunkID)
	}
}
