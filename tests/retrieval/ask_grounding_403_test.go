package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestAsk_RebuildsModelAuthoredSourcesFooter pins issue #403 F2 / SPEC §9.4.1:
// a `Sources:` footer the MODEL wrote must be sanitized too, not just one the
// server appends.
//
// The stripping rule for hallucinated inline citations only reaches bracketed
// [rel_path] tags, so a prose footer listing bare paths slips past it: the model
// can name a document it was never given and the server would emit it verbatim.
// The post-processor must remove the model's footer and rebuild it from the
// in-context citation set.
func TestAsk_RebuildsModelAuthoredSourcesFooter(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})

	gen := &fakeGenerator{out: "Answer body [docs/a.md]\n\nSources: docs/a.md, secret/never-retrieved.pdf"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "docs/a.md",
		Snippet: "alpha snippet",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if strings.Contains(got.Answer, "secret/never-retrieved.pdf") {
		t.Fatalf("model-authored footer named a document that was never in context: %q", got.Answer)
	}
	if !strings.HasPrefix(got.Answer, "Answer body [docs/a.md]") {
		t.Fatalf("expected the answer body preserved, got %q", got.Answer)
	}
	if !strings.HasSuffix(got.Answer, "Sources: [docs/a.md]") {
		t.Fatalf("expected the footer rebuilt from the in-context set, got %q", got.Answer)
	}
	if strings.Count(got.Answer, "Sources:") != 1 {
		t.Fatalf("expected exactly one footer, got %q", got.Answer)
	}
}

// TestAsk_KeepsProseReferencesLineInBody guards the footer-stripping rule
// against over-reach: the scan walks backwards from the end of the answer, so a
// prose line such as "References: RFC 7231" sitting above a bulleted list must
// NOT be mistaken for an attribution footer and take the rest of the answer with
// it. A candidate footer has to name a document (or be a bare label) to qualify.
func TestAsk_KeepsProseReferencesLineInBody(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})

	body := strings.Join([]string{
		"The header semantics are defined upstream [docs/a.md].",
		"References: RFC 7231 covers the caching rules.",
		"",
		"- first consequence",
		"- second consequence",
	}, "\n")
	gen := &fakeGenerator{out: body}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/a.md", Snippet: "alpha"})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(got.Answer, "References: RFC 7231 covers the caching rules.") {
		t.Fatalf("prose References line was stripped as a footer: %q", got.Answer)
	}
	if !strings.Contains(got.Answer, "- second consequence") {
		t.Fatalf("answer body was truncated by footer stripping: %q", got.Answer)
	}
	if !strings.HasSuffix(got.Answer, "Sources: [docs/a.md]") {
		t.Fatalf("expected the rebuilt footer appended after the body, got %q", got.Answer)
	}
}

// TestAsk_StripsBareLabelSourcesFooter covers the other footer shape a model
// emits: a bare "Sources:" label with the documents listed on following lines.
// It must be removed and rebuilt from the in-context set rather than left in
// place beside the server's own footer.
func TestAsk_StripsBareLabelSourcesFooter(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})

	gen := &fakeGenerator{out: "Answer body [docs/a.md]\n\nSources:\n- docs/a.md\n- ghost/unretrieved.pdf"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/a.md", Snippet: "alpha"})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if strings.Contains(got.Answer, "ghost/unretrieved.pdf") {
		t.Fatalf("model-authored footer leaked a document that was never in context: %q", got.Answer)
	}
	if strings.Count(got.Answer, "Sources:") != 1 {
		t.Fatalf("expected exactly one footer, got %q", got.Answer)
	}
	if !strings.HasSuffix(got.Answer, "Sources: [docs/a.md]") {
		t.Fatalf("expected the footer rebuilt from the in-context set, got %q", got.Answer)
	}
}

// TestAsk_ContextSectionStaysWithinBudget pins the per-document budget contract
// of the windowing path: whatever window is selected, the assembled Context
// section must not exceed the configured max_context_chars.
func TestAsk_ContextSectionStaysWithinBudget(t *testing.T) {
	full := strings.Repeat("padding sentence with clause words. ", 200)
	idx := index.NewHNSWIndex("")
	store := &fullTextStore{text: map[uint64]string{}}
	for id := uint64(1); id <= 4; id++ {
		addVec(t, idx, id, []float32{1, float32(id) / 100})
		store.text[id] = full
	}

	for _, budget := range []int{80, 120, 400, 1000} {
		gen := &fakeGenerator{out: "ok"}
		svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
			"mistral-embed": {1, 0},
		}}, gen)
		for id := uint64(1); id <= 4; id++ {
			svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: "docs/d.md", Snippet: "padding"})
		}
		svc.SetMaxContextChars(budget)

		if _, err := svc.Ask(context.Background(), "clause words", model.SearchQuery{K: 4}); err != nil {
			t.Fatalf("Ask(budget=%d) failed: %v", budget, err)
		}
		start := strings.LastIndex(gen.lastPrompt, "Context:\n")
		if start == -1 {
			t.Fatalf("budget=%d: expected a Context section", budget)
		}
		// The Context section ends where the trailing Reminder section begins
		// (issue #892): that reminder is a fixed-size server instruction, outside
		// rag.max_context_chars for the same reason the system prompt is. This
		// assertion is about the documents, so it measures only them.
		ctxSection := gen.lastPrompt[start+len("Context:\n"):]
		// LastIndex, not Cut: the reminder is appended last, and a document could
		// itself contain this literal. A first-match cut would under-measure the
		// context and hide a budget violation.
		if i := strings.LastIndex(ctxSection, "\nReminder:\n"); i >= 0 {
			ctxSection = ctxSection[:i]
		}
		if got := len([]rune(ctxSection)); got > budget {
			t.Fatalf("budget=%d: context section is %d runes, over budget", budget, got)
		}
	}
}

// TestAsk_AbstainsWhenEvidenceIsAbsolutelyWeak pins issue #403 F4 / SPEC §9.4.3:
// when every eligible hit falls below the ABSOLUTE evidence threshold, ask must
// not generate an answer from them. It returns an explicit insufficient-evidence
// answer with an empty citations array, keeping the rejected candidates in
// `hits`.
//
// The relative pruning floor (`retrieval.min_score`) cannot produce this
// outcome: it compares each score against the best of the result set, so the top
// hit is always 1.0 and some hit always clears any floor. Only an absolute
// reading can report that the best hit is itself too weak.
func TestAsk_AbstainsWhenEvidenceIsAbsolutelyWeak(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// Near-orthogonal to the query vector: cosine ~0.02, well under the shipped
	// absolute threshold, while still being the best (and only) hit in the set.
	addVec(t, idx, 1, []float32{0.02, 1})

	gen := &fakeGenerator{out: "a confident, sourced-looking answer [docs/weak.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "docs/weak.md",
		Snippet: "barely related text",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if gen.lastPrompt != "" {
		t.Fatalf("generator must not be invoked on insufficient evidence, prompt was %q", gen.lastPrompt)
	}
	if len(got.Citations) != 0 {
		t.Fatalf("abstention must return an empty citations array, got %d", len(got.Citations))
	}
	if len(got.Hits) == 0 {
		t.Fatal("abstention must still report the rejected candidates in hits")
	}
	if !strings.Contains(got.Answer, "Insufficient evidence") {
		t.Fatalf("expected an explicit insufficient-evidence answer, got %q", got.Answer)
	}
	if strings.Contains(got.Answer, "docs/weak.md") {
		t.Fatalf("abstention must not attribute the rejected candidate, got %q", got.Answer)
	}
}

// TestAsk_AbstentionIsDistinctFromEmptyCorpus pins the second half of §9.4.3: a
// caller MUST be able to tell "I found nothing" apart from "I found material and
// judged it too weak". Both return no citations, so the distinction has to be
// carried somewhere the caller can read.
func TestAsk_AbstentionIsDistinctFromEmptyCorpus(t *testing.T) {
	weakIdx := index.NewHNSWIndex("")
	addVec(t, weakIdx, 1, []float32{0.02, 1})
	weakSvc := retrieval.NewService(nil, weakIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "unused"})
	weakSvc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/weak.md", Snippet: "barely related"})

	abstained, err := weakSvc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask (weak) failed: %v", err)
	}

	emptySvc := retrieval.NewService(nil, index.NewHNSWIndex(""), &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "unused"})
	empty, err := emptySvc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask (empty) failed: %v", err)
	}

	if len(abstained.Citations) != 0 || len(empty.Citations) != 0 {
		t.Fatal("both results must carry an empty citations array")
	}
	if abstained.Answer == empty.Answer {
		t.Fatalf("abstention is indistinguishable from an empty corpus: both answered %q", empty.Answer)
	}
	if len(empty.Hits) != 0 {
		t.Fatalf("empty corpus must report no hits, got %d", len(empty.Hits))
	}
}

// TestAsk_SendsMatchCenteredWindowOfFullChunk pins issue #403 F5 / SPEC §9.4.2:
// the text supplied to the model must contain the region its citation points at.
//
// The prompt used to carry `h.Snippet` (a 240-rune head of the chunk) capped
// again at 300 runes, against chunks running to 2500. A clause past that head
// was never seen by the model even though its citation invited the consumer to
// open that span and verify it. The builder must resolve the FULL chunk text and
// send a match-centered window of it.
func TestAsk_SendsMatchCenteredWindowOfFullChunk(t *testing.T) {
	const (
		headMarker = "HEAD-MARKER-ONLY-IN-THE-FIRST-LINE"
		clause     = "the tenant shall pay the ziggurat levy each quarter"
	)
	// The operative clause sits far past both the 240-rune store snippet and the
	// old 300-rune prompt cap.
	full := headMarker + " " + strings.Repeat("filler padding sentence. ", 70) + clause + " " +
		strings.Repeat("trailing padding sentence. ", 40)
	snippet := string([]rune(full)[:240])

	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	store := &fullTextStore{text: map[uint64]string{1: full}}

	gen := &fakeGenerator{out: "ok [docs/lease.md]"}
	svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "docs/lease.md",
		Snippet: snippet,
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 40},
	})
	// A budget smaller than the chunk forces a window to be selected, which is
	// where a head truncation and a match-centered window differ.
	svc.SetMaxContextChars(700)

	if _, err := svc.Ask(context.Background(), "ziggurat levy", model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask failed: %v", err)
	}

	ctxStart := strings.LastIndex(gen.lastPrompt, "Context:\n")
	if ctxStart == -1 {
		t.Fatalf("expected a Context section, got %q", gen.lastPrompt)
	}
	promptCtx := gen.lastPrompt[ctxStart:]
	if !strings.Contains(promptCtx, clause) {
		t.Fatalf("the matched clause never reached the model: %q", promptCtx)
	}
	if strings.Contains(promptCtx, headMarker) {
		t.Fatalf("expected a match-centered window, got the head of the chunk: %q", promptCtx)
	}
	// The fence must stay intact around the window (issue #445).
	if !strings.Contains(promptCtx, "<<<BEGIN UNTRUSTED DOCUMENT [docs/lease.md]>>>") ||
		!strings.Contains(promptCtx, "<<<END UNTRUSTED DOCUMENT>>>") {
		t.Fatalf("expected a complete untrusted-document fence, got %q", promptCtx)
	}
}

// TestAsk_SendsWholeChunkWhenItFitsTheBudget complements the window test: a
// chunk small enough for its share of the context budget is sent whole, so
// nothing the citation points at is ever dropped (§9.4.2).
func TestAsk_SendsWholeChunkWhenItFitsTheBudget(t *testing.T) {
	full := "opening line. " + strings.Repeat("body sentence. ", 30) + "closing line."

	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	store := &fullTextStore{text: map[uint64]string{1: full}}

	gen := &fakeGenerator{out: "ok"}
	svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "docs/small.md",
		Snippet: "opening line.",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 4},
	})

	if _, err := svc.Ask(context.Background(), "body sentence", model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if !strings.Contains(gen.lastPrompt, full) {
		t.Fatalf("expected the whole chunk in the prompt, got %q", gen.lastPrompt)
	}
}

// TestAsk_DiscardsReplyWhenNoDocumentFitsTheBudget covers the reachable
// misconfiguration where rag_max_context_chars is small but positive.
//
// buildRAGPrompt then fits no complete fenced block, so the prompt's Context
// section is empty and usedIdx is empty. Adopting the model's reply there would
// publish prose grounded in nothing beside an empty citations array, which a
// caller cannot tell apart from a well-sourced answer (SPEC 9.4.1). The budget
// is clamped only against <= 0 and the upper bound, so config can reach this.
func TestAsk_DiscardsReplyWhenNoDocumentFitsTheBudget(t *testing.T) {
	// An 80 char budget cannot hold ONE fenced block: the two fence markers of
	// the block alone run to 74 chars, which leaves 6 for the text and the
	// builder needs 16. Issue #891 made the document count follow the budget,
	// so a budget that starves the prompt is one too small for a single block,
	// not one too small for a fair share across eight hits.
	idx := index.NewHNSWIndex("")
	texts := map[uint64]string{}
	for id := uint64(1); id <= 8; id++ {
		addVec(t, idx, id, []float32{1, 0})
		texts[id] = strings.Repeat("body sentence. ", 30)
	}
	store := &fullTextStore{text: texts}

	fabricated := "The contract terminates on 1 March 2027."
	gen := &fakeGenerator{out: fabricated}
	svc := retrieval.NewService(store, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	for id := uint64(1); id <= 8; id++ {
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: fmt.Sprintf("docs/a%d.md", id), Snippet: "body sentence.",
			Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 4},
		})
	}
	svc.SetMaxContextChars(80) // positive, but under one complete fenced block

	res, err := svc.Ask(context.Background(), "when does it terminate?", model.SearchQuery{K: 8})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if strings.Contains(res.Answer, fabricated) {
		t.Errorf("published an ungrounded reply with no context in the prompt:\n%s", res.Answer)
	}
	if len(res.Citations) != 0 {
		t.Errorf("citations = %d, want 0 when nothing reached the prompt", len(res.Citations))
	}
}
