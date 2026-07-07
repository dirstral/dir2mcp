package retrieval

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestAsk_CitationsRestrictedToInContextChunks pins issue #403 F1: an ask
// answer's citations MUST reference only the chunks actually placed in the
// model's context window. A hit dropped by the context budget was never given
// to the LLM and must not appear as a citation (which the schema marks as
// grounding evidence).
func TestAsk_CitationsRestrictedToInContextChunks(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	addVec(t, idx, 2, []float32{0.9, 0.1})
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "Grounded answer."})
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "docs/b.md",
		Snippet: "beta snippet",
		Span:    model.Span{Kind: "lines", StartLine: 3, EndLine: 4},
	})
	// Budget only fits the first document; the second is never sent to the LLM.
	svc.SetMaxContextChars(110)

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 2})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	// Both hits are still returned on the raw retrieval surface.
	if len(got.Hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(got.Hits))
	}
	// Only the in-context chunk is cited.
	if len(got.Citations) != 1 {
		t.Fatalf("expected 1 citation (in-context only), got %d: %+v", len(got.Citations), got.Citations)
	}
	if got.Citations[0].RelPath != "docs/a.md" {
		t.Fatalf("expected citation for docs/a.md, got %q", got.Citations[0].RelPath)
	}
	if strings.Contains(got.Answer, "[docs/b.md]") {
		t.Fatalf("dropped chunk docs/b.md must not be attributed, got %q", got.Answer)
	}
}

// TestAsk_StripsHallucinatedInlineCitation pins issue #403 F3: an inline
// [rel_path] tag the model emits for a document that is not among the provided
// (in-context) chunks is a hallucination and must be stripped from the
// user-facing answer, while a real citation and non-citation brackets survive.
func TestAsk_StripsHallucinatedInlineCitation(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "Per [docs/a.md] the term is 30 days [1]; see also [secret/lease.pdf#p=12]."})
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if strings.Contains(got.Answer, "secret/lease.pdf") {
		t.Fatalf("hallucinated citation was not stripped: %q", got.Answer)
	}
	if !strings.Contains(got.Answer, "[docs/a.md]") {
		t.Fatalf("real citation was dropped: %q", got.Answer)
	}
	if !strings.Contains(got.Answer, "[1]") {
		t.Fatalf("non-citation bracket [1] must be preserved: %q", got.Answer)
	}
}

// TestStripHallucinatedCitations_Unit exercises the tag-validation heuristic
// directly (issue #403 F3): file-like tags absent from the citation set are
// dropped; matching tags (by full path or basename) and prose brackets stay.
func TestStripHallucinatedCitations_Unit(t *testing.T) {
	citations := []model.Citation{{RelPath: "docs/a.md"}, {RelPath: "reports/q3.pdf"}}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"keeps known full path", "See [docs/a.md].", "See [docs/a.md]."},
		{"keeps known basename", "See [q3.pdf].", "See [q3.pdf]."},
		{"keeps span suffix", "See [reports/q3.pdf#p=3].", "See [reports/q3.pdf#p=3]."},
		{"drops unknown file", "See [ghost.pdf] here.", "See here."},
		{"keeps footnote", "Result [1] holds.", "Result [1] holds."},
		{"keeps prose bracket", "Note [see appendix] please.", "Note [see appendix] please."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripHallucinatedCitations(tc.in, citations)
			if got != tc.want {
				t.Fatalf("stripHallucinatedCitations(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
