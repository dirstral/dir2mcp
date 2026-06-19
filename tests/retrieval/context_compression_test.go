package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// compressionSnippet is a multi-sentence chunk where exactly one sentence is
// on-topic for the query "payment gating", the rest are off-topic filler or
// redundant restatements. An evidence-guided compressor should keep the
// on-topic sentence and drop the noise; with compression off the whole snippet
// is sent verbatim.
const compressionSnippet = "The x402 adapter implements payment gating via an HTTP 402 challenge. " +
	"The office cafeteria serves lunch between noon and two in the afternoon every weekday. " +
	"Quarterly planning meetings are scheduled in the large conference room on the third floor. " +
	"The annual company picnic was rained out last summer and rescheduled twice afterwards. " +
	"Birthday celebrations happen in the kitchen area near the coffee machines on Fridays."

// newCompressionService builds a single-hit vector service whose lone chunk
// carries compressionSnippet. The generator records the prompt it is handed so
// tests can assert on the exact model-facing context.
func newCompressionService(t *testing.T, gen *fakeGenerator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "docs/x402.md",
		Title:   "x402 adapter",
		Snippet: compressionSnippet,
		Span:    model.Span{StartLine: 7, EndLine: 42},
	})
	return svc
}

func askCompression(t *testing.T, svc *retrieval.Service) model.AskResult {
	t.Helper()
	res, err := svc.Ask(context.Background(), "payment gating", model.SearchQuery{Query: "payment gating", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return res
}

// TestAsk_ContextCompression_ShrinksModelContext pins the core win: when
// compression is enabled the prompt handed to the generator is strictly shorter
// than the raw-snippet prompt, AND the kept content still contains the on-topic
// evidence ("payment gating") while the off-topic filler ("cafeteria") is gone.
func TestAsk_ContextCompression_ShrinksModelContext(t *testing.T) {
	rawGen := &fakeGenerator{out: "answer [docs/x402.md]"}
	rawSvc := newCompressionService(t, rawGen)
	rawSvc.SetContextCompression(false, 0)
	_ = askCompression(t, rawSvc)

	compGen := &fakeGenerator{out: "answer [docs/x402.md]"}
	compSvc := newCompressionService(t, compGen)
	compSvc.SetContextCompression(true, 0)
	_ = askCompression(t, compSvc)

	if compGen.lastPrompt == "" || rawGen.lastPrompt == "" {
		t.Fatalf("expected both prompts to be captured")
	}
	if len(compGen.lastPrompt) >= len(rawGen.lastPrompt) {
		t.Fatalf("compressed prompt (%d) must be shorter than raw prompt (%d)",
			len(compGen.lastPrompt), len(rawGen.lastPrompt))
	}
	if !strings.Contains(compGen.lastPrompt, "payment gating") {
		t.Fatalf("compressed prompt must retain the on-topic evidence; got:\n%s", compGen.lastPrompt)
	}
	if strings.Contains(compGen.lastPrompt, "cafeteria") {
		t.Fatalf("compressed prompt must drop off-topic filler; got:\n%s", compGen.lastPrompt)
	}
}

// TestAsk_ContextCompression_PreservesCitations pins citation fidelity: whether
// compression is on or off, the returned citations and hit snippet are
// byte-for-byte identical, because compression only reshapes the prompt copy.
func TestAsk_ContextCompression_PreservesCitations(t *testing.T) {
	rawSvc := newCompressionService(t, &fakeGenerator{out: "a [docs/x402.md]"})
	rawSvc.SetContextCompression(false, 0)
	rawRes := askCompression(t, rawSvc)

	compSvc := newCompressionService(t, &fakeGenerator{out: "a [docs/x402.md]"})
	compSvc.SetContextCompression(true, 0)
	compRes := askCompression(t, compSvc)

	if len(rawRes.Citations) != 1 || len(compRes.Citations) != 1 {
		t.Fatalf("expected exactly one citation each; raw=%d comp=%d",
			len(rawRes.Citations), len(compRes.Citations))
	}
	rc, cc := rawRes.Citations[0], compRes.Citations[0]
	if rc.ChunkID != cc.ChunkID || rc.RelPath != cc.RelPath || rc.Title != cc.Title ||
		rc.Span.StartLine != cc.Span.StartLine || rc.Span.EndLine != cc.Span.EndLine {
		t.Fatalf("citations diverged under compression:\nraw=%+v\ncomp=%+v", rc, cc)
	}
	if cc.Span.StartLine != 7 || cc.Span.EndLine != 42 {
		t.Fatalf("cited span was altered: %+v", cc.Span)
	}
	// The returned hit snippet must remain the full, uncompressed source text.
	if len(compRes.Hits) != 1 || compRes.Hits[0].Snippet != compressionSnippet {
		t.Fatalf("returned hit snippet must be the original uncompressed text; got %q",
			compRes.Hits[0].Snippet)
	}
}

// TestAsk_ContextCompression_DisabledIsUnchanged pins the default-off behavior:
// with compression disabled the prompt contains the full snippet verbatim,
// including the filler the compressor would otherwise drop.
func TestAsk_ContextCompression_DisabledIsUnchanged(t *testing.T) {
	gen := &fakeGenerator{out: "answer [docs/x402.md]"}
	svc := newCompressionService(t, gen)
	svc.SetContextCompression(false, 0)
	_ = askCompression(t, svc)

	if !strings.Contains(gen.lastPrompt, "cafeteria") {
		t.Fatalf("disabled compression must send the full snippet; got:\n%s", gen.lastPrompt)
	}
}

// TestAsk_ContextCompression_DropsRedundantSentences pins the redundancy-dedup
// path: two query-relevant sentences that restate the same evidence collapse to
// one in the model-facing context, so the prompt is shorter than raw while the
// evidence survives once.
func TestAsk_ContextCompression_DropsRedundantSentences(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	// Two near-identical relevant sentences plus filler. The duplicate should be
	// dropped by the redundancy filter.
	snippet := "The payment gating module validates every incoming request token. " +
		"The payment gating module validates each incoming request token carefully. " +
		"Unrelated trivia about the weather and the office plants filled the page."
	gen := &fakeGenerator{out: "a [r.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "r.md", Snippet: snippet})
	svc.SetContextCompression(true, 1) // ratio 1 so budget is not the limiter

	if _, err := svc.Ask(context.Background(), "payment gating request token",
		model.SearchQuery{Query: "payment gating request token", K: 5}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	occurrences := strings.Count(gen.lastPrompt, "payment gating module validates")
	if occurrences != 1 {
		t.Fatalf("redundant relevant sentence should collapse to one; found %d in:\n%s",
			occurrences, gen.lastPrompt)
	}
	if strings.Contains(gen.lastPrompt, "weather") {
		t.Fatalf("off-topic filler should be dropped; got:\n%s", gen.lastPrompt)
	}
}

// TestAsk_ContextCompression_NeverEmptyForTinySnippet pins graceful behavior on
// inputs too short to compress: the prompt still carries the snippet text and
// the citation is intact.
func TestAsk_ContextCompression_NeverEmptyForTinySnippet(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	gen := &fakeGenerator{out: "a [t.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "t.md", Snippet: "short evidence here"})
	svc.SetContextCompression(true, 0.1)

	res, err := svc.Ask(context.Background(), "evidence", model.SearchQuery{Query: "evidence", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(gen.lastPrompt, "short evidence here") {
		t.Fatalf("tiny snippet must pass through unchanged; got:\n%s", gen.lastPrompt)
	}
	if len(res.Citations) != 1 || res.Citations[0].RelPath != "t.md" {
		t.Fatalf("citation must be intact for tiny snippet; got %+v", res.Citations)
	}
}
