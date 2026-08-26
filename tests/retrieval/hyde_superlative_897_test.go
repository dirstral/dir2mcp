package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #897: HyDE measurably helps superlative questions (2/5 to 4/5) and
// degrades three other categories, so a global flag cannot serve every shape.
// The full per-route table (PR #898) failed its audit (19/37 agreement, 0/8 on
// negative controls); superlatives are the one shape the classifier identified
// perfectly (5/5) AND the one shape HyDE helps, so exactly that intersection
// ships: retrieval.hyde.superlative enables HyDE for superlative questions
// only, additively.

// hydeGen answers the HyDE hypothesis prompt, so a HyDE-enabled search calls
// it and a HyDE-disabled search does not: the observable seam.
type hydeGen struct{ calls int }

func (g *hydeGen) Generate(_ context.Context, prompt string) (string, error) {
	g.calls++
	_ = prompt
	return "a short hypothetical answer", nil
}

func superlativeService(t *testing.T, gen *hydeGen) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1, RelPath: "docs/a.md", Snippet: "text",
		Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetHyDESuperlative(true)
	return svc
}

// The graded pilot set's five superlative questions, verbatim: the audit
// measured the classifier at 5/5 on exactly these, which is the evidence the
// feature ships on. If a pattern edit drops one, this fails.
var superlatives897 = []string{
	"What was the fastest pitch of the game?",
	"What was the hardest hit ball?",
	"Who hit the longest home run?",
	"What were the most captivating moments?",
	"Which pitcher threw the most pitches?",
}

// Negative controls from the same graded set: the shapes the #898 audit showed
// a lexical classifier CANNOT identify. None of them may trigger HyDE here,
// which is the entire reason this feature is superlative-only.
var controls897 = []string{
	"Who hit a triple in this game?",
	"What did Shohei Ohtani do in this game?",
	"Which pitcher threw a complete game shutout?",
	"Who was ejected from the game?",
	"What happened in the twelfth inning?",
}

func TestHyDE897_SuperlativeQuestionsGetTheTransform(t *testing.T) {
	for _, q := range superlatives897 {
		gen := &hydeGen{}
		svc := superlativeService(t, gen)
		if _, err := svc.Search(context.Background(), model.SearchQuery{Query: q, K: 1}); err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if gen.calls == 0 {
			t.Errorf("superlative %q did not trigger the HyDE transform", q)
		}
	}
}

func TestHyDE897_NonSuperlativesFollowTheGlobalFlag(t *testing.T) {
	for _, q := range controls897 {
		gen := &hydeGen{}
		svc := superlativeService(t, gen)
		if _, err := svc.Search(context.Background(), model.SearchQuery{Query: q, K: 1}); err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if gen.calls != 0 {
			t.Errorf("non-superlative %q triggered HyDE (%d generator calls); the flag must be additive for superlatives only", q, gen.calls)
		}
	}
}

// TestHyDE897_OffByDefaultIsByteIdentical pins the opt-in: without the flag, a
// superlative question runs exactly the raw-query path.
func TestHyDE897_OffByDefaultIsByteIdentical(t *testing.T) {
	gen := &hydeGen{}
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/a.md", Snippet: "t",
		Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}})
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: superlatives897[0], K: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gen.calls != 0 {
		t.Fatalf("flag off but HyDE ran (%d calls)", gen.calls)
	}
}

// TestHyDE897_NeverTurnsHyDEOff pins the additive contract: with the GLOBAL
// flag on, a non-superlative question still gets HyDE, so enabling the
// superlative flag can never reduce what an operator already chose.
func TestHyDE897_NeverTurnsHyDEOff(t *testing.T) {
	gen := &hydeGen{}
	svc := superlativeService(t, gen)
	svc.SetHyDE(true, "fuse")
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: controls897[0], K: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gen.calls == 0 {
		t.Fatal("global HyDE on, but the transform did not run; the superlative flag must never subtract")
	}
}

// TestHyDE897_FirstAndLastAreNotSuperlatives pins the deliberate absence: they
// read as superlatives of order but collide with time-scoped phrasing, the
// exact collision the #898 audit measured.
func TestHyDE897_FirstAndLastAreNotSuperlatives(t *testing.T) {
	for _, q := range []string{
		"What happened in the first inning?",
		"Describe the last play in the footage.",
	} {
		gen := &hydeGen{}
		svc := superlativeService(t, gen)
		if _, err := svc.Search(context.Background(), model.SearchQuery{Query: q, K: 1}); err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if gen.calls != 0 {
			t.Errorf("%q triggered HyDE; first/last must not classify as superlatives", q)
		}
	}
}
