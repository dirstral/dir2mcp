package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fullTextStore is a minimal model.Store that also exposes the chunk-text
// capability (ChunkTaskByID) used by reranking (issue #399 item 5): it maps a
// chunk id to the FULL chunk text so a test can assert the reranker scores the
// whole chunk, not the truncated snippet carried on the SearchHit.
type fullTextStore struct {
	text map[uint64]string
}

func (s *fullTextStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	full, ok := s.text[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return model.ChunkTask{Label: chunkID, Text: full, Metadata: model.ChunkMetadata{ChunkID: chunkID}}, "", nil
}

func (s *fullTextStore) Init(context.Context) error                           { return nil }
func (s *fullTextStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *fullTextStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *fullTextStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *fullTextStore) Close() error { return nil }

// TestRerank_ScoresFullChunkTextNotSnippet pins issue #399 item 5: the reranker
// must receive each candidate's FULL chunk text, not the ~240-char snippet the
// BM25 path truncates onto the SearchHit. When the store exposes ChunkTaskByID,
// rerankDocs resolves the full text before handing docs to the cross-encoder.
func TestRerank_ScoresFullChunkTextNotSnippet(t *testing.T) {
	idx := index.NewHNSWIndex("")
	for id, vec := range map[uint64][]float32{1: {1, 0}, 2: {0.9, 0.1}} {
		addVec(t, idx, id, vec)
	}
	// Full chunk text is much longer than the truncated snippet the hit carries.
	full1 := "alpha " + strings.Repeat("full-body-one ", 100)
	full2 := "beta " + strings.Repeat("full-body-two ", 100)
	store := &fullTextStore{text: map[uint64]string{1: full1, 2: full2}}

	svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {0, 1},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "a.md", DocType: "md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "b.md", DocType: "md", Snippet: "beta"})

	fr := &fakeReranker{}
	svc.SetReranker(fr, "m", 50)
	svc.SetRerankEnabled(true)

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 2, Index: "text"}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	fr.mu.Lock()
	docs := append([]string(nil), fr.lastDocs...)
	fr.mu.Unlock()
	if len(docs) == 0 {
		t.Fatal("reranker was not called")
	}
	for _, d := range docs {
		// The truncated snippet ("alpha" / "beta") is a prefix of the full text,
		// so require the full-body marker to prove full text was sent, not the
		// bare snippet.
		if !strings.Contains(d, "full-body-") {
			t.Fatalf("reranker received a truncated snippet %q; want full chunk text", d)
		}
	}
}
