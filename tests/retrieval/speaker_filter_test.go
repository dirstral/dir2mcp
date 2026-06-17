package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// addVecSpeaker upserts a vector whose payload carries a diarized "time" span
// speaker, mirroring what payloadFromTask stores in production so the HNSW
// pushdown filter (model.Filter.Match over the payload) sees the attribution.
func addVecSpeaker(t *testing.T, idx *index.HNSWIndex, id uint64, vec []float32, relPath, docType, speaker, label string, startMS, endMS int) {
	t.Helper()
	span := model.Span{Kind: "time", StartMS: startMS, EndMS: endMS, Speaker: speaker, SpeakerLabel: label}
	payload := model.IndexPayload{
		ChunkID: id, RelPath: relPath, DocType: docType,
		StartMS: startMS, EndMS: endMS, Speaker: speaker, SpeakerLabel: label, Span: span,
	}
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", id, err)
	}
}

// TestSearch_SpeakerFilter_RestrictsTimeHits verifies the optional speaker
// filter restricts results to time-spanned transcript hits attributed to that
// speaker (SPEC §8.6.8/§15.2). Hits for other speakers, and non-time chunks, are
// excluded.
func TestSearch_SpeakerFilter_RestrictsTimeHits(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecSpeaker(t, idx, 1, []float32{1, 0}, "interview.mp4", "video", "S1", "Host", 0, 2000)
	addVecSpeaker(t, idx, 2, []float32{0.95, 0.05}, "interview.mp4", "video", "S2", "Guest", 2000, 4000)
	addVecP(t, idx, 3, []float32{0.9, 0.1}, "notes.md", "md")

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "interview.mp4", DocType: "video", Snippet: "host speaks",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 2000, Speaker: "S1", SpeakerLabel: "Host"},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "interview.mp4", DocType: "video", Snippet: "guest speaks",
		Span: model.Span{Kind: "time", StartMS: 2000, EndMS: 4000, Speaker: "S2", SpeakerLabel: "Guest"},
	})
	svc.SetChunkMetadata(3, model.SearchHit{
		RelPath: "notes.md", DocType: "md", Snippet: "no speaker",
		Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 3},
	})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "speaks", K: 10, Speaker: "S2"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("speaker filter S2 should yield exactly 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].ChunkID != 2 || hits[0].Span.Speaker != "S2" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}
}

// TestSearch_SpeakerFilter_CaseInsensitive confirms the speaker filter matches
// case-insensitively (consistent with the other matchFilters predicates).
func TestSearch_SpeakerFilter_CaseInsensitive(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecSpeaker(t, idx, 1, []float32{1, 0}, "a.mp3", "audio", "S1", "", 0, 1000)
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "a.mp3", DocType: "audio", Snippet: "x",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1000, Speaker: "S1"},
	})
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 5, Speaker: "s1"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("case-insensitive speaker filter should match, got %d hits", len(hits))
	}
}

// TestSearch_SpeakerFilter_NoDiarizedCorpus_NoHits confirms that a speaker
// filter over a corpus with no diarized transcripts returns no hits (SPEC
// §8.6.8), without affecting an unfiltered search.
func TestSearch_SpeakerFilter_NoDiarizedCorpus_NoHits(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "plain.mp3", "audio")
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "plain.mp3", DocType: "audio", Snippet: "x",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1000}, // no speaker
	})

	// Filtered: no diarized segment -> no hits.
	filtered, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 5, Speaker: "S1"})
	if err != nil {
		t.Fatalf("Search filtered: %v", err)
	}
	if len(filtered) != 0 {
		t.Fatalf("speaker filter over a non-diarized corpus must return no hits, got %d", len(filtered))
	}

	// Unfiltered: behaviour unchanged (the chunk is still returned).
	unfiltered, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 5})
	if err != nil {
		t.Fatalf("Search unfiltered: %v", err)
	}
	if len(unfiltered) != 1 {
		t.Fatalf("unfiltered search must be unaffected, got %d hits", len(unfiltered))
	}
}

// TestFormatCitation_AppendsSpeaker covers the §9.3 human-readable citation
// format: a diarized time span appends the speaker, and the base form is used
// unchanged when no speaker is present.
func TestFormatCitation_AppendsSpeaker(t *testing.T) {
	cases := []struct {
		name string
		path string
		span model.Span
		want string
	}{
		{
			name: "time with label",
			path: "interview.mp4",
			span: model.Span{Kind: "time", StartMS: 133000, EndMS: 161000, Speaker: "S2", SpeakerLabel: "Guest"},
			want: "[interview.mp4@t=02:13-02:41 › Guest]",
		},
		{
			name: "time with id only",
			path: "interview.mp4",
			span: model.Span{Kind: "time", StartMS: 133000, EndMS: 161000, Speaker: "S2"},
			want: "[interview.mp4@t=02:13-02:41 › S2]",
		},
		{
			name: "time no speaker (base form unchanged)",
			path: "interview.mp4",
			span: model.Span{Kind: "time", StartMS: 133000, EndMS: 161000},
			want: "[interview.mp4@t=02:13-02:41]",
		},
		{
			name: "page span (no speaker surface)",
			path: "doc.pdf",
			span: model.Span{Kind: "page", Page: 4},
			want: "[doc.pdf@p=4]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retrieval.FormatCitation(tc.path, tc.span)
			if got != tc.want {
				t.Errorf("FormatCitation = %q, want %q", got, tc.want)
			}
		})
	}
}
