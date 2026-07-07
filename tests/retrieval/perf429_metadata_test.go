package tests

import (
	"slices"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #429 D1: the retrieval service previously kept a chunk's SearchHit
// metadata in BOTH a per-label map and a redundant per-index (text/code) map,
// resident twice. The per-index split was collapsed into the single chunkByLabel
// store. This test pins that eviction still hides a tombstoned document's chunks
// on the very next search after the collapse — the single map is the sole source
// of truth, and dropping a label from it makes searchHitForLabel fall back to an
// empty-RelPath stub that matchFilters discards.
func TestSearch_SingleMetadataMapEvictionHidesTombstonedChunks(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})

	st := newLivenessStore()
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false) // vector-only ranking; no BM25 store dependency

	// Register both chunks via the per-index entry point (the axis argument is now
	// ignored — metadata is stored once in chunkByLabel).
	svc.SetChunkMetadataForIndex("text", 1, model.SearchHit{ChunkID: 1, RelPath: "keep.md", DocType: "md", Snippet: "kept content"})
	svc.SetChunkMetadataForIndex("code", 2, model.SearchHit{ChunkID: 2, RelPath: "drop.md", DocType: "md", Snippet: "content to drop"})

	got := searchChunkIDs(t, svc, "content")
	if !slices.Contains(got, 1) || !slices.Contains(got, 2) {
		t.Fatalf("expected both chunks searchable before eviction, got %v", got)
	}

	// Whole-document eviction: drop.md is tombstoned in the store.
	svc.EvictDocuments([]string{"drop.md"})

	got = searchChunkIDs(t, svc, "content")
	if slices.Contains(got, 2) {
		t.Fatalf("evicted document's chunk 2 still returned after eviction: %v", got)
	}
	if !slices.Contains(got, 1) {
		t.Fatalf("live chunk 1 should still be returned after evicting a different document, got %v", got)
	}
}
