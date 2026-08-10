package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// payloadOnlyIndex is a test double for an external (Tier C) backend that owns
// the chunk metadata itself: every hit carries its rel_path/doc_type in the
// payload, and the service never registers in-memory metadata for those chunks.
// Delete records the request but removes nothing, which models a native deletion
// that has not propagated yet. SPEC §6.6 requires retrieval to hide a tombstoned
// chunk in exactly that window.
type payloadOnlyIndex struct {
	entries []model.IndexHit
	deleted [][]uint64
}

func newPayloadOnlyIndex(paths ...string) *payloadOnlyIndex {
	idx := &payloadOnlyIndex{}
	for i, relPath := range paths {
		id := uint64(i + 1)
		idx.entries = append(idx.entries, model.IndexHit{
			ChunkID: id,
			Score:   float32(1) - float32(i)/100,
			Payload: model.IndexPayload{ChunkID: id, RelPath: relPath, DocType: "md"},
		})
	}
	return idx
}

func (p *payloadOnlyIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }

func (p *payloadOnlyIndex) Delete(_ context.Context, chunkIDs []uint64) error {
	p.deleted = append(p.deleted, chunkIDs)
	return nil
}

func (p *payloadOnlyIndex) Search(_ context.Context, _ []float32, k int, _ model.Filter) ([]model.IndexHit, error) {
	if k > len(p.entries) {
		k = len(p.entries)
	}
	out := make([]model.IndexHit, k)
	copy(out, p.entries[:k])
	return out, nil
}

func (p *payloadOnlyIndex) Identity(context.Context) (string, error) { return "", nil }
func (p *payloadOnlyIndex) Reset(context.Context, string) error      { return nil }
func (p *payloadOnlyIndex) Close() error                             { return nil }

// deleteRefusingIndex wraps the real HNSW index and fails every Delete. It models
// a backend whose deletion cannot be applied (a network error on an external
// store, for example). The vectors and payloads stay in the index, so the service
// alone must keep the evicted document out of the results.
type deleteRefusingIndex struct {
	*index.HNSWIndex
	calls int
}

func (d *deleteRefusingIndex) Delete(context.Context, []uint64) error {
	d.calls++
	return errors.New("backend refuses deletion")
}

func newEvictionService(t *testing.T, idx model.Index) *retrieval.Service {
	t.Helper()
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false) // vector-only: deterministic, no BM25 store dependency
	return svc
}

// evictSearchPaths runs one search and returns the rel_path of each hit.
func evictSearchPaths(t *testing.T, svc *retrieval.Service, query string) []string {
	t.Helper()
	return searchRelPaths(t, svc, model.SearchQuery{Query: query, K: 10})
}

// TestEvictDocuments_LastDocumentDoesNotReturnFromPayload pins issue #687: the
// corpus holds one document, so evicting it empties the in-memory metadata map.
// The service must still report zero results. Before the fix the empty map made
// the backend payload the source of truth again, and the deleted document came
// back with its rel_path, doc_type and score intact.
func TestEvictDocuments_LastDocumentDoesNotReturnFromPayload(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "docs/deleted.md", "md")

	svc := newEvictionService(t, idx)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/deleted.md", DocType: "md", Snippet: "content"})

	if got := evictSearchPaths(t, svc, "content"); len(got) != 1 {
		t.Fatalf("expected the document before eviction, got %v", got)
	}

	svc.EvictDocument("docs/deleted.md")

	if got := evictSearchPaths(t, svc, "content"); len(got) != 0 {
		t.Fatalf("evicted last document came back from the vector payload: %v", got)
	}

	// The vector is deleted as well, so no orphan stays behind in the backend.
	hits, err := idx.Search(context.Background(), []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("index Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected the vector to be deleted from the index, got %v", hits)
	}
}

// TestEvictDocuments_BatchEvictionOfEveryDocumentReturnsNothing covers the same
// transition through a batch delete: one call removes every currently indexed
// document, so the metadata map empties in one step.
func TestEvictDocuments_BatchEvictionOfEveryDocumentReturnsNothing(t *testing.T) {
	idx := index.NewHNSWIndex("")
	paths := []string{"docs/a.md", "docs/b.md", "docs/c.md"}
	for i, relPath := range paths {
		id := uint64(i + 1)
		addVecP(t, idx, id, []float32{1, float32(i) / 100}, relPath, "md")
	}

	svc := newEvictionService(t, idx)
	for i, relPath := range paths {
		id := uint64(i + 1)
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: relPath, DocType: "md", Snippet: "content"})
	}

	svc.EvictDocuments(paths)

	if got := evictSearchPaths(t, svc, "content"); len(got) != 0 {
		t.Fatalf("batch eviction of every document still returns hits: %v", got)
	}
}

// TestEvictDocuments_PayloadOnlyBackendKeepsLiveDocuments proves the payload
// fallback still works for a backend that is the sole source of chunk metadata:
// the service registers no metadata at all, so live documents must materialise
// from the payload. Only the evicted document is hidden, and it stays hidden even
// though the backend deletion did not remove its vector.
func TestEvictDocuments_PayloadOnlyBackendKeepsLiveDocuments(t *testing.T) {
	idx := newPayloadOnlyIndex("docs/gone.md", "docs/live.md")
	svc := newEvictionService(t, idx)

	if got := evictSearchPaths(t, svc, "content"); len(got) != 2 {
		t.Fatalf("expected both payload documents before eviction, got %v", got)
	}

	svc.EvictDocument("docs/gone.md")

	got := evictSearchPaths(t, svc, "content")
	if len(got) != 1 || got[0] != "docs/live.md" {
		t.Fatalf("expected only docs/live.md after eviction, got %v", got)
	}
}

// TestEvictDocuments_PayloadOnlyBackendBatchEvictionReturnsNothing evicts every
// document of a payload-only backend in one batch. The service holds no metadata
// for those chunks, so the path tombstone is the only state that can hide them.
func TestEvictDocuments_PayloadOnlyBackendBatchEvictionReturnsNothing(t *testing.T) {
	idx := newPayloadOnlyIndex("docs/a.md", "docs/b.md")
	svc := newEvictionService(t, idx)

	svc.EvictDocuments([]string{"docs/a.md", "docs/b.md"})

	if got := evictSearchPaths(t, svc, "content"); len(got) != 0 {
		t.Fatalf("payload-only backend still returns evicted documents: %v", got)
	}
	// The service holds no metadata for these chunks, so it can resolve no
	// chunk_id for the paths and issues no backend delete. The vectors stay in
	// the external store until a reconciliation pass removes them; the tombstone
	// is what upholds the invariant meanwhile.
	if len(idx.deleted) != 0 {
		t.Fatalf("expected no chunk-id delete without in-memory metadata, got %v", idx.deleted)
	}
}

// TestEvictDocuments_HidesDocumentWhenBackendDeleteFails covers a backend that
// cannot apply the deletion. The vector stays in the index, so the invariant of
// SPEC §6.6 has to hold on the read path alone.
func TestEvictDocuments_HidesDocumentWhenBackendDeleteFails(t *testing.T) {
	inner := index.NewHNSWIndex("")
	addVecP(t, inner, 1, []float32{1, 0}, "docs/deleted.md", "md")
	idx := &deleteRefusingIndex{HNSWIndex: inner}

	svc := newEvictionService(t, idx)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/deleted.md", DocType: "md", Snippet: "content"})

	svc.EvictDocument("docs/deleted.md")

	if idx.calls == 0 {
		t.Fatal("expected eviction to ask the backend to delete the vector")
	}
	if got := evictSearchPaths(t, svc, "content"); len(got) != 0 {
		t.Fatalf("evicted document returned after a failed backend delete: %v", got)
	}
	// The vector is still in the backend, so the service, not the backend, is
	// what hides the document.
	hits, err := inner.Search(context.Background(), []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("index Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the vector to survive the refused delete, got %v", hits)
	}
}

// TestEvictDocuments_ReindexClearsTheTombstone pins the un-tombstone rule: a file
// that is deleted and then created again is a live document. Registration of
// metadata for the path makes it visible once more.
func TestEvictDocuments_ReindexClearsTheTombstone(t *testing.T) {
	idx := newPayloadOnlyIndex("docs/recreated.md")
	svc := newEvictionService(t, idx)

	svc.EvictDocument("docs/recreated.md")
	if got := evictSearchPaths(t, svc, "content"); len(got) != 0 {
		t.Fatalf("expected no hits right after eviction, got %v", got)
	}

	// The file comes back and is indexed again.
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/recreated.md", DocType: "md", Snippet: "content"})

	got := evictSearchPaths(t, svc, "content")
	if len(got) != 1 || got[0] != "docs/recreated.md" {
		t.Fatalf("expected the re-indexed document to be searchable, got %v", got)
	}
}
