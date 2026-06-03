package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestSearch_DedupsPageImageWhenTextSurvives pins SPEC 8.1.7: a page-image media
// candidate for (rel_path, page) is dropped when a text/region candidate for the
// same page survives, but media on a page with no competing text is kept.
func TestSearch_DedupsPageImageWhenTextSurvives(t *testing.T) {
	idx := index.NewHNSWIndex("")
	for label, vec := range map[uint64][]float32{
		1: {1, 0},       // text chunk, page 1
		2: {0.99, 0.01}, // page-image media chunk, page 1 (duplicate of #1)
		3: {0.98, 0.02}, // page-image media chunk, page 2 (no competing text)
	} {
		if err := idx.Add(label, vec); err != nil {
			t.Fatalf("idx.Add(%d): %v", label, err)
		}
	}

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "doc.pdf", DocType: "pdf", Snippet: "page one text", Span: model.Span{Kind: "page", Page: 1}})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "doc.pdf", DocType: "pdf", Modality: "pdf", MediaRef: "doc.pdf", Span: model.Span{Kind: "page", Page: 1}})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "doc.pdf", DocType: "pdf", Modality: "pdf", MediaRef: "doc.pdf", Span: model.Span{Kind: "page", Page: 2}})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "page", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[uint64]bool{}
	for _, h := range hits {
		got[h.ChunkID] = true
	}
	if got[2] {
		t.Errorf("page-image duplicate (chunk 2, page 1) must be dropped; hits=%v", got)
	}
	if !got[1] {
		t.Errorf("text chunk (chunk 1) must survive; hits=%v", got)
	}
	if !got[3] {
		t.Errorf("media on a text-free page (chunk 3, page 2) must be kept; hits=%v", got)
	}
}

// TestSearch_KeepsTimeWindowMediaAlongsideTranscript pins SPEC 8.1.7: the dedup
// rule is page-scoped — audio/video time-window media chunks are never dropped,
// even when a transcript text chunk (also a time span) exists for the same file.
func TestSearch_KeepsTimeWindowMediaAlongsideTranscript(t *testing.T) {
	idx := index.NewHNSWIndex("")
	for label, vec := range map[uint64][]float32{
		1: {1, 0},       // transcript text chunk (time span)
		2: {0.99, 0.01}, // audio media window (time span)
	} {
		if err := idx.Add(label, vec); err != nil {
			t.Fatalf("idx.Add(%d): %v", label, err)
		}
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "talk.mp3", DocType: "audio", Snippet: "spoken words", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 120000}})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "talk.mp3", DocType: "audio", Modality: "audio", MediaRef: "talk.mp3", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 120000}})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "talk", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[uint64]bool{}
	for _, h := range hits {
		got[h.ChunkID] = true
	}
	if !got[1] || !got[2] {
		t.Fatalf("both transcript and audio-window chunks must survive (time spans are not page-deduped); hits=%v", got)
	}
}
