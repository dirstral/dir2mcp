package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Answer-time moment grouping (issue #784, dirstral-spec design 0004 §8).
//
// A recognition backend reports one moment once per role. The reference backend
// reports one plate appearance twice: once as `pitch` keyed on the pitcher, and
// once as `at_bat` keyed on the batter. Both annotations carry the same time
// span on the same file.
//
// The split must stay. The v1 wire shape has a flat entity array and one scalar
// event, so a merged annotation cannot say which id acted in which role, and the
// role-exact entity filter of PR #789 dies with it (design 0004 §8). So the
// answer path groups the two on the shared span instead. The generator then
// reads one moment once, and it can no longer report a single walk as two.
//
// The citations stay complete. Both annotations name the same seconds of the
// same file, so both are real provenance for what the answer says.

const (
	walkStartMS = 3725205
	walkEndMS   = 3733205
	slamStartMS = 4110000
	slamEndMS   = 4118000

	batterID  = "player:bryce-eldridge"
	pitcherID = "player:paxton-schultz"
)

// ragBlockOpen is the untrusted-document open marker the prompt builder emits.
// One occurrence is one context document, so counting it counts the moments the
// model was shown.
const ragBlockOpen = "<<<BEGIN UNTRUSTED DOCUMENT"

// countBlocks counts the context documents in a prompt. It counts in the
// Context section only, because the system prompt names the marker as well.
func countBlocks(t *testing.T, prompt string) int {
	t.Helper()
	_, context, ok := strings.Cut(prompt, "\n\nContext:\n")
	if !ok {
		t.Fatalf("prompt has no Context section:\n%s", prompt)
	}
	return strings.Count(context, ragBlockOpen)
}

// walkService builds the pilot corpus situation. Chunks 1 and 2 are the two
// role-keyed annotations of ONE walk, with the identical span. Chunk 3 is a
// different moment in the same video.
func walkService(t *testing.T, gen model.Generator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addAnnotation(t, idx, 1, []float32{1, 0}, "at_bat", []string{batterID}, walkStartMS, walkEndMS)
	addAnnotation(t, idx, 2, []float32{0.99, 0.01}, "pitch", []string{pitcherID}, walkStartMS, walkEndMS)
	addAnnotation(t, idx, 3, []float32{0.98, 0.02}, "at_bat", []string{batterID}, slamStartMS, slamEndMS)

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "game.mp4", DocType: "video",
		Snippet: "At bat: Bryce Eldridge vs Paxton Schultz - Bryce Eldridge walks.",
		Span: model.Span{
			Kind: "time", StartMS: walkStartMS, EndMS: walkEndMS,
			Entities: []string{batterID}, Event: "at_bat",
		},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "game.mp4", DocType: "video",
		Snippet: "Pitch: Paxton Schultz to Bryce Eldridge - Bryce Eldridge walks.",
		Span: model.Span{
			Kind: "time", StartMS: walkStartMS, EndMS: walkEndMS,
			Entities: []string{pitcherID}, Event: "pitch",
		},
	})
	svc.SetChunkMetadata(3, model.SearchHit{
		RelPath: "game.mp4", DocType: "video",
		Snippet: "At bat: Bryce Eldridge vs Paxton Schultz - Bryce Eldridge hits a grand slam.",
		Span: model.Span{
			Kind: "time", StartMS: slamStartMS, EndMS: slamEndMS,
			Entities: []string{batterID}, Event: "at_bat",
		},
	})
	return svc
}

// TestAsk_ShowsOneRoleSplitMomentOnce is the defect. Before the fix the prompt
// carried one block per annotation, so the model saw the walk twice and
// answered that the player "walked twice". The model must now see it once.
func TestAsk_ShowsOneRoleSplitMomentOnce(t *testing.T) {
	gen := &fakeGenerator{out: "Bryce Eldridge walked once and hit a grand slam. [game.mp4]"}
	svc := walkService(t, gen)

	if _, err := svc.Ask(context.Background(), "what did Bryce Eldridge do", model.SearchQuery{K: 10}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := countBlocks(t, gen.lastPrompt); got != 2 {
		t.Fatalf("prompt held %d context blocks, want 2 (the walk and the grand slam)\n%s", got, gen.lastPrompt)
	}
	// One event is one DOCUMENT. Both role-keyed annotations of the walk sit
	// inside the same fenced block, so the generator reads one document for one
	// event and cannot count the walk twice. The block quotes both annotations
	// (issue #890): on a recognition corpus the sibling carries facts the
	// primary lacks, and citing a sibling the model never read is what §9.4.2
	// forbids. What must never happen again is TWO blocks for one moment.
	_, ctxSection, ok := strings.Cut(gen.lastPrompt, "\n\nContext:\n")
	if !ok {
		t.Fatalf("prompt has no Context section:\n%s", gen.lastPrompt)
	}
	walkBlock, _, ok := strings.Cut(ctxSection, "<<<END UNTRUSTED DOCUMENT>>>")
	if !ok {
		t.Fatalf("no closed fence in the context:\n%s", ctxSection)
	}
	if !strings.Contains(walkBlock, "At bat: Bryce Eldridge") ||
		!strings.Contains(walkBlock, "Pitch: Paxton Schultz") {
		t.Fatalf("the two annotations of the walk are not in one block:\n%s", ctxSection)
	}
	if strings.Contains(walkBlock, "grand slam") {
		t.Fatalf("a different moment leaked into the walk's block:\n%s", ctxSection)
	}
	if !strings.Contains(gen.lastPrompt, "grand slam") {
		t.Fatalf("grouping dropped the other moment\n%s", gen.lastPrompt)
	}
}

// TestAsk_KeepsEveryCitationOfAGroupedMoment pins the provenance half of the
// fix. One block reaches the model, but both annotations of that moment stay
// cited: each names the same seconds of the same file, so each opens the
// evidence the answer used.
func TestAsk_KeepsEveryCitationOfAGroupedMoment(t *testing.T) {
	gen := &fakeGenerator{out: "Bryce Eldridge walked once and hit a grand slam. [game.mp4]"}
	svc := walkService(t, gen)

	got, err := svc.Ask(context.Background(), "what did Bryce Eldridge do", model.SearchQuery{K: 10})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) != 3 {
		t.Fatalf("got %d citations, want 3 (both roles of the walk, plus the grand slam)", len(got.Citations))
	}
	cited := make(map[uint64]model.Span, len(got.Citations))
	for _, c := range got.Citations {
		cited[c.ChunkID] = c.Span
	}
	for _, id := range []uint64{1, 2, 3} {
		if _, ok := cited[id]; !ok {
			t.Fatalf("chunk %d is missing from the citations: %+v", id, got.Citations)
		}
	}
	// The spans must survive byte for byte. Grouping never rewrites a span.
	for _, id := range []uint64{1, 2} {
		if cited[id].StartMS != walkStartMS || cited[id].EndMS != walkEndMS {
			t.Fatalf("chunk %d cites span %d-%d, want %d-%d",
				id, cited[id].StartMS, cited[id].EndMS, walkStartMS, walkEndMS)
		}
	}
	// The hits are the retrieval result, so they stay untouched as well.
	if len(got.Hits) != 3 {
		t.Fatalf("got %d hits, want the 3 retrieved chunks", len(got.Hits))
	}
}

// TestFallbackAnswer_ListsOneRoleSplitMomentOnce covers the path with no
// generator. That answer is user facing too, and it listed the same moment
// twice for the same reason.
func TestFallbackAnswer_ListsOneRoleSplitMomentOnce(t *testing.T) {
	svc := walkService(t, nil)

	got, err := svc.Ask(context.Background(), "what did Bryce Eldridge do", model.SearchQuery{K: 10})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if n := strings.Count(got.Answer, "walks."); n != 1 {
		t.Fatalf("the fallback answer states the walk %d times, want 1\n%s", n, got.Answer)
	}
}

// TestAsk_KeepsDistinctMomentsApart guards the fix against over-reach. Two
// annotations that report different seconds are two moments, so they must stay
// two blocks even when their text is alike.
func TestAsk_KeepsDistinctMomentsApart(t *testing.T) {
	gen := &fakeGenerator{out: "answer [game.mp4]"}
	idx := index.NewHNSWIndex("")
	addAnnotation(t, idx, 1, []float32{1, 0}, "at_bat", []string{batterID}, walkStartMS, walkEndMS)
	addAnnotation(t, idx, 2, []float32{0.99, 0.01}, "at_bat", []string{batterID}, walkStartMS, walkEndMS+1000)

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "game.mp4", DocType: "video", Snippet: "first walk",
		Span: model.Span{
			Kind: "time", StartMS: walkStartMS, EndMS: walkEndMS,
			Entities: []string{batterID}, Event: "at_bat",
		},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "game.mp4", DocType: "video", Snippet: "second walk",
		Span: model.Span{
			Kind: "time", StartMS: walkStartMS, EndMS: walkEndMS + 1000,
			Entities: []string{batterID}, Event: "at_bat",
		},
	})

	if _, err := svc.Ask(context.Background(), "how many walks", model.SearchQuery{K: 10}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := countBlocks(t, gen.lastPrompt); got != 2 {
		t.Fatalf("prompt held %d context blocks, want 2 (two different moments)\n%s", got, gen.lastPrompt)
	}
}

// TestAsk_GroupsOnlyRecognitionAnnotations keeps the rule narrow. Two chunks
// with no recognition attribution can share a time span, for example a
// transcript window and an OCR window over the same seconds. They carry
// different text, so they stay two documents and the model reads both.
func TestAsk_GroupsOnlyRecognitionAnnotations(t *testing.T) {
	gen := &fakeGenerator{out: "answer [talk.mp4]"}
	idx := index.NewHNSWIndex("")
	addVecP(t, idx, 1, []float32{1, 0}, "talk.mp4", "video")
	addVecP(t, idx, 2, []float32{0.99, 0.01}, "talk.mp4", "video")

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	span := model.Span{Kind: "time", StartMS: 1000, EndMS: 9000}
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "talk.mp4", DocType: "video", Snippet: "the spoken words", Span: span,
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "talk.mp4", DocType: "video", Snippet: "the words on the slide", Span: span,
	})

	if _, err := svc.Ask(context.Background(), "what was said", model.SearchQuery{K: 10}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := countBlocks(t, gen.lastPrompt); got != 2 {
		t.Fatalf("prompt held %d context blocks, want 2 (no attribution, so no grouping)\n%s", got, gen.lastPrompt)
	}
}
