package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// countingEmbedder685 wraps the shared retrieval embedder fake and counts the
// Embed calls. The adaptive skip verdict must answer without any retrieval, so
// the query embedding is a cost the skip path must not pay (#685).
type countingEmbedder685 struct {
	inner *fakeRetrievalEmbedder
	calls int
}

func (e *countingEmbedder685) Embed(
	ctx context.Context, modelName string, role model.EmbedRole, texts []string,
) ([][]float32, error) {
	e.calls++
	return e.inner.Embed(ctx, modelName, role, texts)
}

// newSkipService685 builds a retrieval service with the adaptive gate enabled,
// a counting embedder and the supplied generator. lastK stays -1 when the index
// is never queried.
func newSkipService685(
	t *testing.T, gen model.Generator,
) (*retrieval.Service, *fakeRetrievalIndex, *countingEmbedder685) {
	t.Helper()
	idx := &fakeRetrievalIndex{lastK: -1}
	emb := &countingEmbedder685{
		inner: &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}},
	}
	svc := retrieval.NewService(nil, idx, emb, gen)
	svc.SetOverfetchMultiplier(1)
	svc.SetAdaptiveRetrieval(true, 4, 30)
	return svc, idx, emb
}

// noContextFallback685 is the zero-hit fallback answer. A skip verdict must
// never return it: retrieval did not run, so the corpus was never consulted and
// the message misreports the server's own state.
const noContextFallback685 = "No relevant context found in the indexed corpus."

// TestAdaptiveSkip685_CallsGeneratorWithoutRetrieval pins the fix for #685. An
// adaptive skip verdict must answer through the configured generator with a
// non-RAG prompt. It must not query the index, must not embed the query, and
// must not return the empty-corpus fallback.
func TestAdaptiveSkip685_CallsGeneratorWithoutRetrieval(t *testing.T) {
	gen := &fakeGenerator{out: "Hello. Ask me a question about the indexed documents."}
	svc, idx, emb := newSkipService685(t, gen)

	res, err := svc.Ask(context.Background(), "hello", model.SearchQuery{Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if idx.lastK != -1 {
		t.Fatalf("skip verdict must not query the index, got k=%d", idx.lastK)
	}
	if emb.calls != 0 {
		t.Fatalf("skip verdict must not embed the query, got %d Embed call(s)", emb.calls)
	}
	if res.Answer == noContextFallback685 {
		t.Fatalf("skip verdict returned the empty-corpus fallback instead of a generated answer")
	}
	if res.Answer != gen.out {
		t.Fatalf("answer = %q, want the generator output %q", res.Answer, gen.out)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("a no-retrieval answer must carry no citations, got %d", len(res.Citations))
	}
	if len(res.Hits) != 0 {
		t.Fatalf("a no-retrieval answer must carry no hits, got %d", len(res.Hits))
	}
	if gen.lastPrompt == "" {
		t.Fatalf("the generator was never called")
	}
	if !strings.Contains(gen.lastPrompt, "hello") {
		t.Fatalf("the non-RAG prompt must carry the message, got %q", gen.lastPrompt)
	}
	if strings.Contains(gen.lastPrompt, "UNTRUSTED DOCUMENT") {
		t.Fatalf("the no-retrieval prompt must carry no document fence, got %q", gen.lastPrompt)
	}
}

// TestAdaptiveSkip685_NoGeneratorFallback pins the no-generator case. Without a
// generator the server still must not claim that it searched the corpus.
func TestAdaptiveSkip685_NoGeneratorFallback(t *testing.T) {
	svc, idx, emb := newSkipService685(t, nil)

	res, err := svc.Ask(context.Background(), "thanks", model.SearchQuery{Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if idx.lastK != -1 {
		t.Fatalf("skip verdict must not query the index, got k=%d", idx.lastK)
	}
	if emb.calls != 0 {
		t.Fatalf("skip verdict must not embed the query, got %d Embed call(s)", emb.calls)
	}
	if res.Answer == noContextFallback685 {
		t.Fatalf("skip verdict returned the empty-corpus fallback with no generator")
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Fatalf("skip verdict returned an empty answer")
	}
	if len(res.Citations) != 0 {
		t.Fatalf("a no-retrieval answer must carry no citations, got %d", len(res.Citations))
	}
}

// TestAdaptiveSkip685_GeneratorErrorFallsBack pins the failure path. A broken
// generator must degrade to the deterministic no-retrieval reply, not to the
// empty-corpus message, and must still emit no citations.
func TestAdaptiveSkip685_GeneratorErrorFallsBack(t *testing.T) {
	gen := &fakeGenerator{err: errors.New("provider down")}
	svc, _, _ := newSkipService685(t, gen)

	res, err := svc.Ask(context.Background(), "ok", model.SearchQuery{Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer == noContextFallback685 {
		t.Fatalf("a failed no-retrieval generation returned the empty-corpus fallback")
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Fatalf("a failed no-retrieval generation returned an empty answer")
	}
	if len(res.Citations) != 0 {
		t.Fatalf("a no-retrieval answer must carry no citations, got %d", len(res.Citations))
	}
}

// TestAdaptiveSkip685_StripsFabricatedSources pins the grounding rule of SPEC
// §9.4.1 on the no-retrieval path. The model saw no document, so any file tag
// or Sources footer it writes is ungrounded and must not reach the caller.
func TestAdaptiveSkip685_StripsFabricatedSources(t *testing.T) {
	gen := &fakeGenerator{out: "Hello, see [docs/a.md].\n\nSources: [docs/a.md]"}
	svc, _, _ := newSkipService685(t, gen)

	res, err := svc.Ask(context.Background(), "hi", model.SearchQuery{Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if strings.Contains(res.Answer, "docs/a.md") {
		t.Fatalf("a no-retrieval answer must cite nothing, got %q", res.Answer)
	}
	if strings.Contains(res.Answer, "Sources:") {
		t.Fatalf("a no-retrieval answer must carry no Sources footer, got %q", res.Answer)
	}
	if len(res.Citations) != 0 {
		t.Fatalf("a no-retrieval answer must carry no citations, got %d", len(res.Citations))
	}
}

// TestAdaptiveSkip685_SubstantiveQuestionNeverSkips pins the guard on the
// no-retrieval path. A caller can set the retrieval query apart from the
// question. A trivial query override must not remove the evidence from a real
// question: the server must still retrieve, and the generator must not answer
// the question with no document.
func TestAdaptiveSkip685_SubstantiveQuestionNeverSkips(t *testing.T) {
	gen := &fakeGenerator{out: "x402 is a payment challenge protocol."}
	svc, idx, emb := newSkipService685(t, gen)

	res, err := svc.Ask(
		context.Background(),
		"how does the x402 payment challenge work?",
		model.SearchQuery{Index: "text", Query: "hi"},
	)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if idx.lastK == -1 {
		t.Fatalf("a substantive question must still query the index, even with a trivial query override")
	}
	if emb.calls == 0 {
		t.Fatalf("a substantive question must still embed the query")
	}
	if gen.lastPrompt != "" {
		t.Fatalf("a zero-hit retrieved ask must not call the generator")
	}
	if res.Answer != noContextFallback685 {
		t.Fatalf("answer = %q, want the empty-corpus fallback for a zero-hit retrieved ask", res.Answer)
	}
}

// TestAdaptiveSkip685_TrivialQuestionWithRichQueryRetrieves pins the other half
// of the guard. The k decision still follows the retrieval query, so a rich
// query override keeps its retrieval and its widened k.
func TestAdaptiveSkip685_TrivialQuestionWithRichQueryRetrieves(t *testing.T) {
	svc, idx, _ := newSkipService685(t, nil)

	hard := "compare and contrast the performance and reliability tradeoffs between approach one and approach two in detail"
	if _, err := svc.Ask(context.Background(), "hi", model.SearchQuery{Index: "text", Query: hard}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if idx.lastK != 30 {
		t.Fatalf("a hard query override must still widen k to k_max=30, got %d", idx.lastK)
	}
}

// TestAdaptiveSkip685_RetrievedAskUnchanged pins the scope of the fix. A real
// question still retrieves, still generates from the retrieved context, and
// still returns its citations.
func TestAdaptiveSkip685_RetrievedAskUnchanged(t *testing.T) {
	gen := &fakeGenerator{out: "The answer is in [docs/a.md]."}
	svc, idx, emb := newSkipService685(t, gen)

	res, err := svc.Ask(context.Background(), "what is x402?", model.SearchQuery{Index: "text"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if idx.lastK == -1 {
		t.Fatalf("a real question must still query the index")
	}
	if emb.calls == 0 {
		t.Fatalf("a real question must still embed the query")
	}
	// The fake index returns no hits, so the retrieved path keeps the
	// empty-corpus fallback and never calls the generator.
	if res.Answer != noContextFallback685 {
		t.Fatalf("retrieved ask with no hits: answer = %q, want the empty-corpus fallback", res.Answer)
	}
	if gen.lastPrompt != "" {
		t.Fatalf("a zero-hit retrieved ask must not call the generator")
	}
}
