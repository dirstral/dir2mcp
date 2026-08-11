package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #691: cross-file dedup (SPEC 9.2) grouped candidates with a rel_path →
// content_hash map that was loaded once, before ingest started, and never
// updated. A live daemon therefore grouped on the corpus as it was at startup.
// The user-visible symptom is result loss: an edited file that USED to be a
// byte-identical copy of another file stays suppressed in `search`/`ask` until
// the daemon restarts, so a relevant and now distinct document is missing from
// the results and from the citations.
//
// The tests below drive the five transitions from the issue against one running
// service. They never restart it and they never rebuild it.

// newLiveDedupService builds a vector-only retrieval service with cross-file
// dedup on and the given startup snapshot installed, mirroring what the server
// does before it starts the ingestor.
func newLiveDedupService(hashes []model.DocumentHash) (*retrieval.Service, *index.HNSWIndex) {
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetCrossFileDedupEnabled(true)
	svc.SetDocumentHashes(hashes)
	return svc, idx
}

// addDedupChunk registers one candidate chunk: a vector plus the chunk metadata
// that carries its rel_path.
func addDedupChunk(t *testing.T, svc *retrieval.Service, idx *index.HNSWIndex, id uint64, vec []float32, relPath string) {
	t.Helper()
	addVec(t, idx, id, vec)
	svc.SetChunkMetadata(id, model.SearchHit{RelPath: relPath, Snippet: "alpha"})
}

// dedupSearchPaths returns the rel_path of every surviving hit, in order.
func dedupSearchPaths(t *testing.T, svc *retrieval.Service) []string {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	paths := make([]string, 0, len(hits))
	for _, h := range hits {
		paths = append(paths, h.RelPath)
	}
	return paths
}

func assertDedupPaths(t *testing.T, got, want []string, stage string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: want %v, got %v", stage, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: want %v, got %v", stage, want, got)
		}
	}
}

// TestSearch_CrossFileDedup_LiveNewDuplicateCollapses covers transition 1: a
// byte-identical copy is added while the daemon runs. Before ingest reports it,
// its path is unknown and both copies are returned. After ingest reports it, the
// pair collapses to one survivor without a restart.
func TestSearch_CrossFileDedup_LiveNewDuplicateCollapses(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "before the copy is indexed")

	svc.UpdateDocumentHash("copy/a.md", "H1")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "after the copy is indexed")
}

// TestSearch_CrossFileDedup_LiveDuplicateBecomesDistinct covers transition 2,
// the result-loss case. One of two duplicates is edited, so the two documents
// are distinct now. Both must be returned, because a stale group must never
// suppress a document that no longer belongs to it.
func TestSearch_CrossFileDedup_LiveDuplicateBecomesDistinct(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "while both copies are identical")

	svc.UpdateDocumentHash("copy/a.md", "H2")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "after one copy is edited")
}

// TestSearch_CrossFileDedup_LiveDistinctBecomesDuplicate covers transition 3:
// one file is edited until it matches another. The pair must collapse on the
// next query, and the best-ranked document must be the survivor (SPEC 9.2).
func TestSearch_CrossFileDedup_LiveDistinctBecomesDuplicate(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "b.md", ContentHash: "H2"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "b.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "b.md"}, "while the files differ")

	svc.UpdateDocumentHash("b.md", "H1")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "after b.md is edited to match a.md")
}

// TestSearch_CrossFileDedup_LiveInitialIngestIsGrouped covers transition 4: the
// snapshot is taken before the first scan, so every document of a fresh corpus
// is unknown to it. Dedup must still group the documents this session indexes.
func TestSearch_CrossFileDedup_LiveInitialIngestIsGrouped(t *testing.T) {
	svc, idx := newLiveDedupService(nil)
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "before the first scan reports")

	svc.UpdateDocumentHash("a.md", "H1")
	svc.UpdateDocumentHash("copy/a.md", "H1")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "after the first scan reports")
}

// TestSearch_CrossFileDedup_LiveDeletionForgetsGroupKey covers transition 5. A
// deleted document must give up its group key. Here the path comes back with new
// content and its new hash is not published yet: the document must pass through
// un-grouped, never be suppressed against the content it held before.
func TestSearch_CrossFileDedup_LiveDeletionForgetsGroupKey(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "b.md", ContentHash: "H1"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "b.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "while both files are identical")

	// a.md is removed from the corpus, then re-created with new content.
	svc.EvictDocuments([]string{"a.md"})
	addDedupChunk(t, svc, idx, 3, []float32{0.98, 0.02}, "a.md")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"b.md", "a.md"}, "after a.md is deleted and re-created")
}

// TestSearch_CrossFileDedup_LiveWithheldHashStopsSuppression pins the in-flight
// window. Ingest blanks a document's content_hash while it rewrites the
// document's representations (#402), so the service must forget the group key
// for that window and restore it when the write commits.
func TestSearch_CrossFileDedup_LiveWithheldHashStopsSuppression(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	svc.UpdateDocumentHash("copy/a.md", "")
	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "while the hash is withheld")

	svc.UpdateDocumentHash("copy/a.md", "H1")
	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md"}, "after the write commits")
}

// TestSearch_CrossFileDedup_LiveUpdateIgnoredWhenDisabled pins the default-off
// contract: live updates must not switch dedup on for a corpus that did not ask
// for it.
func TestSearch_CrossFileDedup_LiveUpdateIgnoredWhenDisabled(t *testing.T) {
	svc, idx := newLiveDedupService(nil)
	svc.SetCrossFileDedupEnabled(false)
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	svc.UpdateDocumentHash("a.md", "H1")
	svc.UpdateDocumentHash("copy/a.md", "H1")

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "dedup disabled")
}

// TestSearch_CrossFileDedup_LiveSetDocumentHashesReplacesPending pins that a
// full reload states the whole truth: it drops buffered updates instead of
// letting them reappear after the reload.
func TestSearch_CrossFileDedup_LiveSetDocumentHashesReplacesPending(t *testing.T) {
	svc, idx := newLiveDedupService(nil)
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	svc.UpdateDocumentHash("a.md", "H1")
	svc.UpdateDocumentHash("copy/a.md", "H1")
	svc.SetDocumentHashes([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H2"},
	})

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "after a full reload")
}

// TestSearch_CrossFileDedup_LiveConcurrentUpdatesAndQueries drives queries and
// ingest updates at the same time, so `go test -race` proves the map is safe to
// share. The end state is asserted after the writers finish, so the assertion
// stays deterministic.
func TestSearch_CrossFileDedup_LiveConcurrentUpdatesAndQueries(t *testing.T) {
	svc, idx := newLiveDedupService([]model.DocumentHash{
		{RelPath: "a.md", ContentHash: "H1"},
		{RelPath: "copy/a.md", ContentHash: "H1"},
	})
	addDedupChunk(t, svc, idx, 1, []float32{1, 0}, "a.md")
	addDedupChunk(t, svc, idx, 2, []float32{0.99, 0.01}, "copy/a.md")

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				svc.UpdateDocumentHash("copy/a.md", "H2")
				svc.UpdateDocumentHash("noise.md", "H9")
				svc.EvictDocuments([]string{"noise.md"})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10}); err != nil {
					t.Errorf("Search during live ingest: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	assertDedupPaths(t, dedupSearchPaths(t, svc), []string{"a.md", "copy/a.md"}, "after concurrent updates")
}
