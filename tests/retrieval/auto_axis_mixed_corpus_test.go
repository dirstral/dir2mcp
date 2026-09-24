package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// SPEC §9.1 (0.73.0): under index=auto a query that is not code-oriented
// searches BOTH axes on a corpus that holds text and code chunks. Before, auto
// defaulted to text, so a plain-English question about a repository ("how is
// the signature verified?") never reached a single code chunk.
func TestSearchWithAxis_AutoFollowsTheCorpusComposition(t *testing.T) {
	newSvc := func(axes ...string) *retrieval.Service {
		idx := &fakeRetrievalIndex{lastK: -1}
		emb := &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}
		svc := retrieval.NewService(nil, idx, emb, nil)
		for i, axis := range axes {
			svc.SetChunkMetadataForIndex(axis, uint64(i+1), model.SearchHit{ChunkID: uint64(i + 1), RelPath: "f" + axis})
		}
		return svc
	}
	prose := model.SearchQuery{Index: "auto", Query: "how is the signature verified"}
	codeShaped := model.SearchQuery{Index: "auto", Query: "func verify() {"}
	for _, tc := range []struct {
		name  string
		svc   *retrieval.Service
		query model.SearchQuery
		want  string
	}{
		{"mixed corpus, prose question searches both", newSvc("text", "code"), prose, "both"},
		{"mixed corpus, code-shaped query narrows to code", newSvc("text", "code"), codeShaped, "code"},
		{"text-only corpus stays on text", newSvc("text", "text"), prose, "text"},
		{"code-only corpus searches code", newSvc("code"), prose, "code"},
		{"empty corpus keeps the old default", newSvc(), prose, "text"},
		{"explicit text is still honoured on a mixed corpus", newSvc("text", "code"), model.SearchQuery{Index: "text", Query: "how is the signature verified"}, "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := tc.svc.SearchWithAxis(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("SearchWithAxis: %v", err)
			}
			if got != tc.want {
				t.Errorf("axis = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSearchWithAxis_EvictionUpdatesTheComposition: once the last code chunk is
// evicted the corpus is text-only again, and auto goes back to text.
func TestSearchWithAxis_EvictionUpdatesTheComposition(t *testing.T) {
	idx := &fakeRetrievalIndex{lastK: -1}
	emb := &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}
	svc := retrieval.NewService(nil, idx, emb, nil)
	svc.SetChunkMetadataForIndex("text", 1, model.SearchHit{ChunkID: 1, RelPath: "notes.md"})
	svc.SetChunkMetadataForIndex("code", 2, model.SearchHit{ChunkID: 2, RelPath: "main.go"})
	prose := model.SearchQuery{Index: "auto", Query: "how is the signature verified"}
	if _, got, _ := svc.SearchWithAxis(context.Background(), prose); got != "both" {
		t.Fatalf("before eviction axis = %q, want both", got)
	}
	svc.EvictDocuments([]string{"main.go"})
	if _, got, _ := svc.SearchWithAxis(context.Background(), prose); got != "text" {
		t.Errorf("after evicting the only code file axis = %q, want text", got)
	}
}
