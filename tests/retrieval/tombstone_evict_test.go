package tests

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// livenessStore is a minimal model.Store that also implements the retrieval
// chunk-liveness capability (ChunkTaskByID): a chunk is live unless its id is in
// the tombstoned set, in which case the lookup returns model.ErrNotFound — the
// same signal *store.SQLiteStore emits for a chunk soft-deleted by a partial
// incremental reindex (SoftDeleteChunksFromOrdinal). Safe for concurrent use.
type livenessStore struct {
	mu         sync.Mutex
	tombstoned map[uint64]bool
}

func newLivenessStore() *livenessStore {
	return &livenessStore{tombstoned: map[uint64]bool{}}
}

func (s *livenessStore) tombstone(id uint64) {
	s.mu.Lock()
	s.tombstoned[id] = true
	s.mu.Unlock()
}

func (s *livenessStore) revive(id uint64) {
	s.mu.Lock()
	delete(s.tombstoned, id)
	s.mu.Unlock()
}

func (s *livenessStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if chunkID == 0 || s.tombstoned[chunkID] {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Metadata: model.ChunkMetadata{ChunkID: chunkID}}, "", nil
}

func (s *livenessStore) Init(context.Context) error                           { return nil }
func (s *livenessStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *livenessStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *livenessStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *livenessStore) Close() error { return nil }

// TestSearch_EvictsTombstonedChunksAfterPartialReindex pins issue #409 item (a):
// an in-place edit that shrinks a document tombstones its trailing chunks via
// SoftDeleteChunksFromOrdinal with no whole-document eviction. Retrieval must
// stop returning that content on the very next search — without a restart — and
// evict the label/vector from the in-memory index so it never resurfaces.
func TestSearch_EvictsTombstonedChunksAfterPartialReindex(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// Two chunks of one document; chunk 2 is the trailing chunk a later edit removes.
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})

	st := newLivenessStore()
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false) // vector-only: deterministic ranking, no BM25 store dependency
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "doc.md", DocType: "md", Snippet: "kept content"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "doc.md", DocType: "md", Snippet: "deleted trailing content"})

	// Before the edit both chunks are searchable.
	got := searchChunkIDs(t, svc, "content")
	if !slices.Contains(got, 2) {
		t.Fatalf("expected chunk 2 present before deletion, got %v", got)
	}

	// Partial reindex: chunk 2 is soft-deleted (SoftDeleteChunksFromOrdinal).
	st.tombstone(2)

	// The first search after the edit must already exclude the deleted content.
	got = searchChunkIDs(t, svc, "content")
	if slices.Contains(got, 2) {
		t.Fatalf("deleted chunk 2 still returned after incremental edit (no restart): %v", got)
	}
	if !slices.Contains(got, 1) {
		t.Fatalf("live chunk 1 should still be returned, got %v", got)
	}

	// Prove the eviction was durable (label + vector dropped from the index),
	// not merely a per-query filter: even if the store reports chunk 2 live
	// again, it must stay absent because its vector was deleted from the index.
	st.revive(2)
	got = searchChunkIDs(t, svc, "content")
	if slices.Contains(got, 2) {
		t.Fatalf("evicted chunk 2 reappeared; expected it dropped from the in-memory index, got %v", got)
	}
}

// TestSearch_TombstoneEvictionRaceSafe exercises the eviction path concurrently
// with searches (run under -race): the tombstone prune writes chunkByLabel and
// the indexes while other goroutines read them, so any missing lock surfaces as
// a data race. Correctness is also asserted — no search may ever return the
// tombstoned chunk.
func TestSearch_TombstoneEvictionRaceSafe(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})

	st := newLivenessStore()
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "doc.md", DocType: "md", Snippet: "kept content"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "doc.md", DocType: "md", Snippet: "deleted content"})

	st.tombstone(2)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "content", K: 10})
				if err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				for _, h := range hits {
					if h.ChunkID == 2 {
						t.Errorf("tombstoned chunk 2 returned under concurrency")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
