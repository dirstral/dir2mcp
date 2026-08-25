package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issues #896 and #785, spec 0.55.0: the server already computed an absolute
// evidence verdict in order to abstain; no client could see it. These tests pin
// the exposure: a named verdict per hit, and an aggregate on the ask result,
// one vocabulary, never a raw score.

// verdictService builds a one-embedder service over vectors chosen so the
// query's cosine against each chunk is the value the test names.
func verdictService(t *testing.T, gen *fakeGenerator, vecs map[uint64][]float32) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for id, v := range vecs {
		addVec(t, idx, id, v)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	for id := range vecs {
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: "docs/a.md", Snippet: "text",
			Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
		})
	}
	return svc
}

// TestSearch896_HitsCarryTheNamedVerdict is the #785 half: each search hit
// names its own absolute verdict, so a caller can read relevance without
// comparing raw scores across modes.
func TestSearch896_HitsCarryTheNamedVerdict(t *testing.T) {
	svc := verdictService(t, &fakeGenerator{out: "ok"}, map[uint64][]float32{
		1: {1, 0},    // cosine ~1.0, far above the 0.05 threshold
		2: {0.02, 1}, // cosine ~0.02, below it
	})
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", K: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[uint64]string{}
	for _, h := range hits {
		got[h.ChunkID] = h.EvidenceVerdict
	}
	if got[1] != "sufficient" {
		t.Fatalf("strong cosine hit verdict = %q, want sufficient", got[1])
	}
	if got[2] != "insufficient" {
		t.Fatalf("near-orthogonal hit verdict = %q, want insufficient", got[2])
	}
}

// TestAsk896_AbstentionCarriesInsufficient is the #896 half: the structured
// form of abstention. Before this, a caller had to parse the answer text.
func TestAsk896_AbstentionCarriesInsufficient(t *testing.T) {
	gen := &fakeGenerator{out: "must not run"}
	svc := verdictService(t, gen, map[uint64][]float32{1: {0.02, 1}})
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "insufficient" {
		t.Fatalf("abstention verdict = %q, want insufficient", got.EvidenceVerdict)
	}
	if len(got.Citations) != 0 || gen.lastPrompt != "" {
		t.Fatal("abstention semantics changed: citations or generation appeared")
	}
}

// TestAsk896_AnsweredAskCarriesTheAggregate pins the normative aggregation: the
// strongest eligible hit's verdict, so one strong hit among weak ones reads
// sufficient.
func TestAsk896_AnsweredAskCarriesTheAggregate(t *testing.T) {
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	svc := verdictService(t, gen, map[uint64][]float32{
		1: {1, 0},
		2: {0.02, 1},
	})
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" {
		t.Fatalf("answered verdict = %q, want sufficient (strongest eligible hit)", got.EvidenceVerdict)
	}
}
