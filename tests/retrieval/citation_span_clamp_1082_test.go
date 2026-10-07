package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #1082: an inline citation tag can name the right document with a span
// the model was never shown. #403 strips a tag for an unshown DOCUMENT; it
// did not read the span. A client that opens the made-up span shows the user
// text the model did not read, which is the overstated grounding SPEC §9.4.1
// forbids ("inline citation tags MUST resolve to that same in-context set").
//
// The rule under test: a tag resolves to a shown block of its document, and
// the span it names may only narrow that block's span. A span inside the
// shown range stays as written. A span that reaches outside is clamped to the
// part the model saw, and a span with nothing in common with any shown block
// becomes that block's whole span. The rewritten tag is the §9.3 form the
// block header carried, so a client that matched the header still matches.

// clampService indexes one chunk per span, all for the same document, and
// answers with the fixed text so the test can read what the server published.
func clampService(t *testing.T, answer string, spans ...model.Span) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for i := range spans {
		addVec(t, idx, uint64(i+1), []float32{1, 0})
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: answer})
	for i, span := range spans {
		svc.SetChunkMetadata(uint64(i+1), model.SearchHit{
			ChunkID: uint64(i + 1),
			RelPath: "docs/Normans.md",
			Snippet: "The Normans were originally Vikings who settled in Normandy.",
			Span:    span,
		})
	}
	return svc
}

func askClamp(t *testing.T, answer string, spans ...model.Span) string {
	t.Helper()
	svc := clampService(t, answer, spans...)
	got, err := svc.Ask(t.Context(), "who were the Normans?", model.SearchQuery{K: len(spans)})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return got.Answer
}

// TestAsk1082_TagOutsideTheShownBlockIsClampedToIt is the reproduction. The
// model was shown lines 25-33 and cites lines 40-48, which it never read.
func TestAsk1082_TagOutsideTheShownBlockIsClampedToIt(t *testing.T) {
	answer := askClamp(t, "They settled in Normandy [docs/Normans.md:L40-L48].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if strings.Contains(answer, "L40-L48") {
		t.Fatalf("the answer still names lines the model was not shown: %q", answer)
	}
	if !strings.Contains(answer, "[docs/Normans.md:L25-L33]") {
		t.Fatalf("the tag was not clamped to the shown block: %q", answer)
	}
}

// A range that starts inside the block and runs past its end keeps the part
// the model saw and loses the rest.
func TestAsk1082_PartialOverlapKeepsTheShownPart(t *testing.T) {
	answer := askClamp(t, "They settled in Normandy [docs/Normans.md:L30-L40].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[docs/Normans.md:L30-L33]") {
		t.Fatalf("partial overlap was not clamped to the intersection: %q", answer)
	}
}

// The issue's own example: [Normans.md:L32-33] against a block tagged
// [Normans.md:L25-L33]. The lines lie inside the block, so the model did read
// them, and the tag stays byte for byte, non-canonical spelling included.
func TestAsk1082_TagInsideTheShownBlockStaysAsWritten(t *testing.T) {
	answer := askClamp(t, "They settled in Normandy [Normans.md:L32-33].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[Normans.md:L32-33]") {
		t.Fatalf("a tag inside the shown block was rewritten: %q", answer)
	}
}

// The normal case: the model copied the header. Nothing changes.
func TestAsk1082_ExactCopyOfTheHeaderIsUnchanged(t *testing.T) {
	answer := askClamp(t, "They settled in Normandy [docs/Normans.md:L25-L33].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[docs/Normans.md:L25-L33].") {
		t.Fatalf("an exact copy of the header was rewritten: %q", answer)
	}
}

// A bare tag names no span, so it cannot name an unseen one. It stays.
func TestAsk1082_BareTagIsUnchanged(t *testing.T) {
	answer := askClamp(t, "They settled in Normandy [docs/Normans.md].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[docs/Normans.md].") {
		t.Fatalf("a bare tag was rewritten: %q", answer)
	}
}

// The legacy @L spelling (#942) is read the same way: inside stays, outside
// is clamped.
func TestAsk1082_LegacyLineSpellingIsReadToo(t *testing.T) {
	inside := askClamp(t, "They settled [docs/Normans.md@L26-30].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(inside, "[docs/Normans.md@L26-30]") {
		t.Fatalf("a legacy tag inside the block was rewritten: %q", inside)
	}
	outside := askClamp(t, "They settled [docs/Normans.md@L50-60].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(outside, "[docs/Normans.md:L25-L33]") {
		t.Fatalf("a legacy tag outside the block was not clamped: %q", outside)
	}
}

// A footnote marker and bracketed prose are not citations and stay.
func TestAsk1082_NonCitationBracketsAreUntouched(t *testing.T) {
	answer := askClamp(t, "Result [1] holds [see appendix] [docs/Normans.md:L40-L48].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[1]") || !strings.Contains(answer, "[see appendix]") {
		t.Fatalf("non-citation brackets were touched: %q", answer)
	}
}

// Two blocks of one document were shown. The tag resolves to the block it
// overlaps, and a tag that overlaps neither resolves to the nearest one.
func TestAsk1082_TwoShownBlocksResolveByOverlapThenDistance(t *testing.T) {
	first := model.Span{Kind: "lines", StartLine: 1, EndLine: 10}
	second := model.Span{Kind: "lines", StartLine: 20, EndLine: 30}

	overlap := askClamp(t, "Cited [docs/Normans.md:L22-L40].", first, second)
	if !strings.Contains(overlap, "[docs/Normans.md:L22-L30]") {
		t.Fatalf("the tag did not resolve to the overlapping block: %q", overlap)
	}
	gap := askClamp(t, "Cited [docs/Normans.md:L12-L15].", first, second)
	if !strings.Contains(gap, "[docs/Normans.md:L1-L10]") {
		t.Fatalf("a tag in the gap did not resolve to the nearest block: %q", gap)
	}
}

// A page tag for a page the model was not shown is clamped to the shown page.
func TestAsk1082_PageTagIsClamped(t *testing.T) {
	wrong := askClamp(t, "See [docs/Normans.md#p=7].", model.Span{Kind: "page", Page: 3})
	if !strings.Contains(wrong, "[docs/Normans.md#p=3]") {
		t.Fatalf("a page tag for an unshown page was not clamped: %q", wrong)
	}
	right := askClamp(t, "See [docs/Normans.md#p=3].", model.Span{Kind: "page", Page: 3})
	if !strings.Contains(right, "[docs/Normans.md#p=3].") {
		t.Fatalf("a correct page tag was rewritten: %q", right)
	}
}

// A time tag follows the same rule, so a media citation never names a moment
// the model was not shown. The shown moment is 02:02:10-02:02:31.
func TestAsk1082_TimeTagIsClamped(t *testing.T) {
	shown := model.Span{Kind: "time", StartMS: 7330000, EndMS: 7351000}

	outside := askClamp(t, "Riley Park homered [docs/Normans.md@t=02:03:00-02:03:30].", shown)
	if !strings.Contains(outside, "[docs/Normans.md@t=02:02:10-02:02:31]") {
		t.Fatalf("a time tag outside the shown moment was not clamped: %q", outside)
	}
	partial := askClamp(t, "Riley Park homered [docs/Normans.md@t=02:02:00-02:02:20].", shown)
	if !strings.Contains(partial, "[docs/Normans.md@t=02:02:10-02:02:20]") {
		t.Fatalf("a time tag reaching before the shown moment was not clamped: %q", partial)
	}
	inside := askClamp(t, "Riley Park homered [docs/Normans.md@t=02:02:15-02:02:20].", shown)
	if !strings.Contains(inside, "[docs/Normans.md@t=02:02:15-02:02:20]") {
		t.Fatalf("a time tag inside the shown moment was rewritten: %q", inside)
	}
	exact := askClamp(t, "Riley Park homered [docs/Normans.md@t=02:02:10-02:02:31].", shown)
	if !strings.Contains(exact, "[docs/Normans.md@t=02:02:10-02:02:31].") {
		t.Fatalf("an exact copy of the time header was rewritten: %q", exact)
	}
}

// The header renders whole seconds, so a model that copies it names a start
// up to 999 ms before the chunk's real start. That copy is the shown span,
// not a span outside it, and must stay byte for byte.
func TestAsk1082_SubSecondStartDoesNotMakeACopyLookOutside(t *testing.T) {
	shown := model.Span{Kind: "time", StartMS: 7330500, EndMS: 7351000}
	answer := askClamp(t, "Riley Park homered [docs/Normans.md@t=02:02:10-02:02:31].", shown)
	if !strings.Contains(answer, "[docs/Normans.md@t=02:02:10-02:02:31].") {
		t.Fatalf("a copy of the rounded header was rewritten: %q", answer)
	}
}

// A diarized moment carries its speaker; a clamped tag carries it too, in the
// §9.3 form the header used.
func TestAsk1082_ClampedTimeTagKeepsTheSpeaker(t *testing.T) {
	shown := model.Span{Kind: "time", StartMS: 133000, EndMS: 161000, Speaker: "S2"}
	answer := askClamp(t, "She said so [docs/Normans.md@t=05:00-05:30 › S2].", shown)
	if !strings.Contains(answer, "[docs/Normans.md@t=02:13-02:41 › S2]") {
		t.Fatalf("the clamped diarized tag lost its form: %q", answer)
	}
}

// A tag whose span kind differs from the shown block's kind names nothing
// the model saw, so it becomes the block's own tag.
func TestAsk1082_WrongKindBecomesTheShownTag(t *testing.T) {
	answer := askClamp(t, "See [docs/Normans.md#p=2].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	if !strings.Contains(answer, "[docs/Normans.md:L25-L33]") {
		t.Fatalf("a page tag against a line block was not clamped: %q", answer)
	}
}

// The structured citations are untouched: they already carry the real span of
// every shown block (#403 F1), and the wire shape does not change.
func TestAsk1082_StructuredCitationsKeepTheRealSpan(t *testing.T) {
	svc := clampService(t, "They settled [docs/Normans.md:L40-L48].",
		model.Span{Kind: "lines", StartLine: 25, EndLine: 33})
	got, err := svc.Ask(t.Context(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) != 1 {
		t.Fatalf("citations = %d, want 1", len(got.Citations))
	}
	if c := got.Citations[0]; c.Span.StartLine != 25 || c.Span.EndLine != 33 {
		t.Fatalf("structured citation span = %+v, want L25-L33", c.Span)
	}
}
