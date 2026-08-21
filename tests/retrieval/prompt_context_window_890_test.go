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

// Answer-prompt context window (issues #890 and #891).
//
// #890. Moment grouping (#784) collapses the annotations that share one time
// span into one moment. The prompt carried the text of the best-ranked member
// only, and the answer cited EVERY member. So a citation named text the model
// never read, which SPEC §9.4.2 forbids and which the truncation path of #403
// already refuses to do. On a recognition corpus the members are complementary,
// not redundant: the exit velocity, the distance and the running score of an
// at-bat live in members that the prompt dropped. `ask` then denied a fact and
// cited the chunk that states it.
//
// #891. A fixed cap of 8 documents clamped the prompt, so `k` above 8 grew the
// citation list only. Every superlative over more than 8 moments answered the
// maximum of a sample of 8.
//
// Both are one code path: the loop in buildRAGPrompt that turns moments into
// fenced context blocks.

const (
	pilotFile     = "giants.mp4"
	chapmanStart  = 3600000
	chapmanEnd    = 3608000
	exitVelocity  = "exit velocity 107 mph"
	homeRunLength = "distance 421 ft"
)

// annotationChunk is one recognition annotation: a chunk id, its rank (the
// vector is derived from it), its span and its text.
type annotationChunk struct {
	id      uint64
	event   string
	startMS int
	endMS   int
	text    string
}

// buildAnnotationService indexes the given annotations in the order supplied.
// The vectors decay with position, so annotation i ranks i-th for the query
// vector {1, 0}. Every chunk carries a recognition attribution, so chunks that
// share a span group into one moment.
func buildAnnotationService(t *testing.T, gen model.Generator, chunks []annotationChunk) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for i, c := range chunks {
		vec := []float32{1, float32(i) / 1000}
		addAnnotation(t, idx, c.id, vec, c.event, []string{"player:matt-chapman"}, c.startMS, c.endMS)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	for _, c := range chunks {
		svc.SetChunkMetadata(c.id, model.SearchHit{
			ChunkID: c.id, RelPath: pilotFile, DocType: "video", Snippet: c.text,
			Span: model.Span{
				Kind: "time", StartMS: c.startMS, EndMS: c.endMS,
				Entities: []string{"player:matt-chapman"}, Event: c.event,
			},
		})
	}
	return svc
}

// chapmanMoment is the measured case of #890: six annotations of ONE home run,
// on one span, each carrying different facts. The at-bat ranks first, so it was
// the only member the prompt ever held. The exit velocity, the distance, the
// captivating index and the running score sat in the five members it dropped.
func chapmanMoment() []annotationChunk {
	return []annotationChunk{
		{id: 1, event: "at_bat", startMS: chapmanStart, endMS: chapmanEnd,
			text: "At bat: Matt Chapman vs Foster Griffin (bottom of the 6th) - Matt Chapman homers (5)."},
		{id: 2, event: "batted_ball", startMS: chapmanStart, endMS: chapmanEnd,
			text: "Batted ball: Matt Chapman vs Foster Griffin (bottom of the 6th): " +
				exitVelocity + ", launch angle 22 degrees, " + homeRunLength + ", fly ball."},
		{id: 3, event: "captivating", startMS: chapmanStart, endMS: chapmanEnd,
			text: "Captivating moment (captivating index 38): Matt Chapman homers to left center field."},
		{id: 4, event: "home_run", startMS: chapmanStart, endMS: chapmanEnd,
			text: "Home run: Matt Chapman off Foster Griffin, his fifth of the season."},
		{id: 5, event: "pitch", startMS: chapmanStart, endMS: chapmanEnd,
			text: "Pitch: Foster Griffin to Matt Chapman (bottom of the 6th), four-seam fastball 93 mph."},
		{id: 6, event: "scoring_play", startMS: chapmanStart, endMS: chapmanEnd,
			text: "Scoring play (1 RBI, score: away 6, home 1): Matt Chapman homers."},
	}
}

// promptContext returns the Context section of the prompt the generator saw.
func promptContext(t *testing.T, prompt string) string {
	t.Helper()
	_, ctxSection, ok := strings.Cut(prompt, "\n\nContext:\n")
	if !ok {
		t.Fatalf("prompt has no Context section:\n%s", prompt)
	}
	// The Context section ends where the trailing Reminder section begins
	// (issue #892). The reminder is a fixed-size server instruction, outside
	// rag.max_context_chars for the same reason the system prompt is: the budget
	// bounds the RETRIEVED context, and the assertions below are about the
	// documents. Cutting here keeps this test measuring exactly that.
	if before, _, found := strings.Cut(ctxSection, "\nReminder:\n"); found {
		return before
	}
	return ctxSection
}

// TestAsk890_CitesOnlyTextTheModelWasShown is the #890 invariant: the citation
// set is a subset of what the prompt actually placed. Before the fix the five
// dropped members of the Chapman moment were cited without ever being placed.
func TestAsk890_CitesOnlyTextTheModelWasShown(t *testing.T) {
	chunks := chapmanMoment()
	gen := &fakeGenerator{out: "Matt Chapman homered in the sixth. [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chunks)

	got, err := svc.Ask(context.Background(), "what happened on Matt Chapman's home run",
		model.SearchQuery{K: 10})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) == 0 {
		t.Fatal("the answer carries no citation at all")
	}
	textByID := make(map[uint64]string, len(chunks))
	for _, c := range chunks {
		textByID[c.id] = c.text
	}
	ctxSection := promptContext(t, gen.lastPrompt)
	for _, c := range got.Citations {
		text, ok := textByID[c.ChunkID]
		if !ok {
			t.Fatalf("citation names an unknown chunk %d", c.ChunkID)
		}
		if !strings.Contains(ctxSection, text) {
			t.Fatalf("chunk %d is cited but its text never reached the prompt.\ncited text: %s\ncontext:\n%s",
				c.ChunkID, text, ctxSection)
		}
	}
}

// TestAsk890_PlacesTheMemberThatHoldsTheAnswer is the measured consequence. The
// exit velocity lives in a NON-primary member of the moment. The model must
// read it, otherwise `ask` denies a fact that its own sixth citation states.
func TestAsk890_PlacesTheMemberThatHoldsTheAnswer(t *testing.T) {
	gen := &fakeGenerator{out: "107 mph. [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chapmanMoment())

	if _, err := svc.Ask(context.Background(),
		"What was the exit velocity on Matt Chapman's home run in the sixth inning?",
		model.SearchQuery{K: 12}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	ctxSection := promptContext(t, gen.lastPrompt)
	if !strings.Contains(ctxSection, exitVelocity) {
		t.Fatalf("the exit velocity never reached the model:\n%s", ctxSection)
	}
	if !strings.Contains(ctxSection, homeRunLength) {
		t.Fatalf("the distance never reached the model:\n%s", ctxSection)
	}
}

// TestAsk890_KeepsOneMomentInOneFencedBlock keeps the #784 fix. The members of
// one moment share ONE untrusted-document block, so the generator still reads
// one event once and cannot count a role-split annotation as two events.
func TestAsk890_KeepsOneMomentInOneFencedBlock(t *testing.T) {
	gen := &fakeGenerator{out: "ok [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chapmanMoment())

	if _, err := svc.Ask(context.Background(), "what happened", model.SearchQuery{K: 12}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := countBlocks(t, gen.lastPrompt); got != 1 {
		t.Fatalf("prompt held %d context blocks, want 1 (six annotations of one moment)\n%s",
			got, gen.lastPrompt)
	}
	ctxSection := promptContext(t, gen.lastPrompt)
	block, _, ok := strings.Cut(ctxSection, "<<<END UNTRUSTED DOCUMENT>>>")
	if !ok {
		t.Fatalf("no closed fence in the context:\n%s", ctxSection)
	}
	for _, want := range []string{exitVelocity, "captivating index 38", "1 RBI"} {
		if !strings.Contains(block, want) {
			t.Fatalf("member text %q sits outside the moment's block:\n%s", want, ctxSection)
		}
	}
}

// TestAsk890_DropsTheCitationOfAMemberTheBudgetExcluded pins the other half of
// the invariant. When the character budget cannot hold every member, the
// members left out must not be cited either.
func TestAsk890_DropsTheCitationOfAMemberTheBudgetExcluded(t *testing.T) {
	chunks := chapmanMoment()
	gen := &fakeGenerator{out: "ok [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chunks)
	// Room for the fence and roughly one annotation, not for six.
	svc.SetMaxContextChars(220)

	got, err := svc.Ask(context.Background(), "what happened", model.SearchQuery{K: 12})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) == 0 {
		t.Fatal("the answer carries no citation at all")
	}
	if len(got.Citations) == len(chunks) {
		t.Fatalf("every member is cited, but the %d char budget cannot hold them all", 220)
	}
	// The citation set must be exactly the members the block quotes: no member
	// the budget excluded, and no member it kept.
	ctxSection := promptContext(t, gen.lastPrompt)
	want := make(map[uint64]bool, len(chunks))
	for _, c := range chunks {
		if strings.Contains(ctxSection, c.text) {
			want[c.id] = true
		}
	}
	cited := make(map[uint64]bool, len(got.Citations))
	for _, c := range got.Citations {
		cited[c.ChunkID] = true
	}
	for id := range want {
		if !cited[id] {
			t.Fatalf("chunk %d is quoted in the prompt but is not cited:\n%s", id, ctxSection)
		}
	}
	for id := range cited {
		if !want[id] {
			t.Fatalf("chunk %d is cited but the budget kept its text out of the prompt:\n%s",
				id, ctxSection)
		}
	}
}

// TestAsk890_CitesADuplicateMemberItCollapsed keeps provenance complete for the
// case the grouping was written for. Two role-keyed annotations of one moment
// can carry identical text; the prompt states it once, and both annotations
// stay cited because that one copy IS their text.
func TestAsk890_CitesADuplicateMemberItCollapsed(t *testing.T) {
	const shared = "Bryce Eldridge walks on four pitches."
	chunks := []annotationChunk{
		{id: 1, event: "at_bat", startMS: chapmanStart, endMS: chapmanEnd, text: shared},
		{id: 2, event: "pitch", startMS: chapmanStart, endMS: chapmanEnd, text: shared},
	}
	gen := &fakeGenerator{out: "ok [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chunks)

	got, err := svc.Ask(context.Background(), "what did Bryce Eldridge do", model.SearchQuery{K: 10})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) != 2 {
		t.Fatalf("got %d citations, want 2 (both roles of one moment)", len(got.Citations))
	}
	if n := strings.Count(promptContext(t, gen.lastPrompt), shared); n != 1 {
		t.Fatalf("identical member text is stated %d times in the prompt, want 1", n)
	}
}

// TestAsk891_PlacesEveryMomentTheBudgetAffords is #891. The corpus holds 20
// captivating moments and the strongest one ranks 15th, past the old cap of 8
// documents. The old prompt held 8, so the answer named the best of a sample.
// The character budget, not a fixed count, must decide now.
func TestAsk891_PlacesEveryMomentTheBudgetAffords(t *testing.T) {
	const moments = 20
	// The true maximum sits at rank 15, well past the old cap.
	const bestRank = 15
	chunks := make([]annotationChunk, 0, moments)
	for i := 0; i < moments; i++ {
		index := 10 + i
		if i == bestRank-1 {
			index = 95
		}
		chunks = append(chunks, annotationChunk{
			id: uint64(i + 1), event: "captivating", startMS: 60000 * i, endMS: 60000*i + 8000,
			text: fmt.Sprintf("Captivating moment (captivating index %d): play %d of the game.", index, i+1),
		})
	}
	gen := &fakeGenerator{out: "The most captivating moment is play 15. [" + pilotFile + "]"}
	svc := buildAnnotationService(t, gen, chunks)

	got, err := svc.Ask(context.Background(), "what was the most captivating moment of the game",
		model.SearchQuery{K: moments})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if n := countBlocks(t, gen.lastPrompt); n != moments {
		t.Fatalf("prompt held %d context blocks, want %d: a superlative answered from a sample", n, moments)
	}
	if !strings.Contains(promptContext(t, gen.lastPrompt), "captivating index 95") {
		t.Fatalf("the true maximum never reached the model:\n%s", promptContext(t, gen.lastPrompt))
	}
	if len(got.Citations) != moments {
		t.Fatalf("got %d citations, want %d", len(got.Citations), moments)
	}
}

// TestAsk891_ContextSectionStaysWithinBudget pins the bound that replaces the
// document cap. Whatever the moment count, the assembled Context section must
// not exceed rag.max_context_chars.
func TestAsk891_ContextSectionStaysWithinBudget(t *testing.T) {
	const moments = 40
	body := strings.Repeat("captivating clause words of the play. ", 20)
	chunks := make([]annotationChunk, 0, moments)
	for i := 0; i < moments; i++ {
		chunks = append(chunks, annotationChunk{
			id: uint64(i + 1), event: "captivating", startMS: 60000 * i, endMS: 60000*i + 8000,
			text: fmt.Sprintf("play %d: %s", i+1, body),
		})
	}
	for _, budget := range []int{300, 1000, 4000, 20000} {
		gen := &fakeGenerator{out: "ok"}
		svc := buildAnnotationService(t, gen, chunks)
		svc.SetMaxContextChars(budget)
		if _, err := svc.Ask(context.Background(), "clause words", model.SearchQuery{K: moments}); err != nil {
			t.Fatalf("Ask(budget=%d): %v", budget, err)
		}
		if got := len([]rune(promptContext(t, gen.lastPrompt))); got > budget {
			t.Fatalf("budget=%d: context section is %d runes, over budget", budget, got)
		}
		// A budget under one document share must still ground the answer. 40
		// candidates sharing 300 chars would leave 7 chars each, so the count
		// follows the budget down to one document rather than starving.
		if n := countBlocks(t, gen.lastPrompt); n < 1 {
			t.Fatalf("budget=%d: no document reached the prompt", budget)
		}
	}
}

// TestAsk891_StillAbstainsOnWeakEvidence guards the widened window against the
// failure it could hide. More context must never turn "I cannot answer" into a
// confident guess: the insufficient-evidence guard (§9.4.3) runs on the hits,
// before the prompt is built, so it fires whatever the document count is.
func TestAsk891_StillAbstainsOnWeakEvidence(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// 40 candidates, every one near-orthogonal to the query: cosine ~0.02,
	// under the shipped absolute threshold.
	for i := 0; i < 40; i++ {
		addAnnotation(t, idx, uint64(i+1), []float32{0.02, 1}, "captivating",
			[]string{"player:matt-chapman"}, 60000*i, 60000*i+8000)
	}
	fabricated := "The most captivating moment was the grand slam."
	gen := &fakeGenerator{out: fabricated}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	for i := 0; i < 40; i++ {
		svc.SetChunkMetadata(uint64(i+1), model.SearchHit{
			ChunkID: uint64(i + 1), RelPath: pilotFile, DocType: "video",
			Snippet: fmt.Sprintf("Captivating moment (captivating index %d).", 10+i),
			Span: model.Span{
				Kind: "time", StartMS: 60000 * i, EndMS: 60000*i + 8000,
				Entities: []string{"player:matt-chapman"}, Event: "captivating",
			},
		})
	}

	got, err := svc.Ask(context.Background(), "what was the most captivating moment",
		model.SearchQuery{K: 40})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if strings.Contains(got.Answer, fabricated) {
		t.Fatalf("weak evidence produced a confident answer: %q", got.Answer)
	}
	if len(got.Citations) != 0 {
		t.Fatalf("abstention must carry no citation, got %d", len(got.Citations))
	}
}
