package tests

import (
	"context"
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
