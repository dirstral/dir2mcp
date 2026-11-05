package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// SPEC §8.6.3 (0.87.0), "Translated cues follow the source speech": these tests
// pin how a translation with no word timings of its own is timed against the
// source words under broadcast segmentation.

// pausedSource is one source segment [0, 9000] with a 5.1 s pause between the
// end of "four" (1900) and the start of "five" (7000). Its second segment opens
// at 20000, so the marker window of the first one is [0, 20000).
func pausedSource() []subtitle.TranscriptChunk {
	return []subtitle.TranscriptChunk{
		{Text: "one two three four five six seven eight", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 9000, Words: []model.WordSpan{
			{T: 0, D: 400, W: "one"}, {T: 500, D: 400, W: "two"}, {T: 1000, D: 400, W: "three"}, {T: 1500, D: 400, W: "four"},
			{T: 7000, D: 400, W: "five"}, {T: 7500, D: 400, W: "six"}, {T: 8000, D: 400, W: "seven"}, {T: 8500, D: 500, W: "eight"},
		}}},
		{Text: "nine", Span: model.Span{Kind: "time", StartMS: 20000, EndMS: 20500, Words: []model.WordSpan{
			{T: 20000, D: 500, W: "nine"},
		}}},
	}
}

// pausedTranslation is the chat translation of pausedSource: no word timings,
// and each segment carries its source segment's start_ms and spoken end.
func pausedTranslation() []subtitle.Cue {
	return subtitle.BuildCues([]subtitle.TranscriptChunk{
		{Text: "uno dos tres cuatro cinco seis siete ocho", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 9000}},
		{Text: "nueve", Span: model.Span{Kind: "time", StartMS: 20000, EndMS: 20500}},
	})
}

// overlapsPause reports whether a cue covers any time inside (from, to).
func overlapsPause(c subtitle.Cue, from, to int) bool {
	return c.StartMS < to && c.EndMS > from
}

// cueWith returns the first cue whose text holds word, failing the test when
// none does.
func cueWith(t *testing.T, cues []subtitle.Cue, word string) subtitle.Cue {
	t.Helper()
	for _, c := range cues {
		for _, tok := range strings.Fields(strings.ReplaceAll(c.Text, "\n", " ")) {
			if tok == word {
				return c
			}
		}
	}
	t.Fatalf("no cue holds %q: %+v", word, cues)
	return subtitle.Cue{}
}

// TestReflowTranslatedCuesNoCueInSourcePause pins the core rule: with a 5.1 s
// source pause, no translated cue covers the pause. The first four translated
// tokens stay in the first spoken run, the last four in the second, and the
// cue that opens the second run does not lead in to the pause.
func TestReflowTranslatedCuesNoCueInSourcePause(t *testing.T) {
	out := subtitle.ReflowTranslatedCues(pausedTranslation(), pausedSource(), 2000)
	if len(out) < 3 {
		t.Fatalf("want at least 3 cues (two runs plus the next segment), got %d: %+v", len(out), out)
	}
	for _, c := range out {
		if overlapsPause(c, 1900, 7000) {
			t.Errorf("cue %+v covers the source pause (1900, 7000)", c)
		}
	}
	for _, w := range []string{"uno", "dos", "tres", "cuatro"} {
		if c := cueWith(t, out, w); c.EndMS > 1900 {
			t.Errorf("%q is in cue %+v, which ends after the first run (1900)", w, c)
		}
	}
	for _, w := range []string{"cinco", "seis", "siete", "ocho"} {
		if c := cueWith(t, out, w); c.StartMS < 7000 || c.EndMS > 20000 {
			t.Errorf("%q is in cue %+v, outside the second run [7000, 20000)", w, c)
		}
	}
	if got, want := cueText(out), "uno dos tres cuatro cinco seis siete ocho nueve"; got != want {
		t.Fatalf("text changed: got %q, want %q", got, want)
	}

	// The old per-segment spreading puts translated words in the pause. This
	// proves the fixture exercises the rule.
	old := subtitle.ReflowChunkCues(pausedTranslation())
	covered := false
	for _, c := range old {
		covered = covered || overlapsPause(c, 1900, 7000)
	}
	if !covered {
		t.Fatalf("fixture is too weak: ReflowChunkCues output %+v does not cover the pause", old)
	}
}

// TestReflowTranslatedCuesSplitsTokensBySourceWords pins the token shares: two
// source words then six source words give 1 and 3 of 4 translated tokens.
func TestReflowTranslatedCuesSplitsTokensBySourceWords(t *testing.T) {
	src := []subtitle.TranscriptChunk{{Text: "a b c d e f g h", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 9000, Words: []model.WordSpan{
		{T: 0, D: 400, W: "a"}, {T: 500, D: 400, W: "b"},
		{T: 4000, D: 400, W: "c"}, {T: 4500, D: 400, W: "d"}, {T: 5000, D: 400, W: "e"},
		{T: 5500, D: 400, W: "f"}, {T: 6000, D: 400, W: "g"}, {T: 6500, D: 400, W: "h"},
	}}}}
	tr := []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 6900, Text: "Alpha bravo charlie delta"}}
	out := subtitle.ReflowTranslatedCues(tr, src, 2000)
	if len(out) != 2 {
		t.Fatalf("want 2 cues (one per spoken run), got %d: %+v", len(out), out)
	}
	if out[0].Text != "Alpha" || out[0].EndMS > 4000 {
		t.Errorf("first run cue = %+v, want text %q ending by 4000", out[0], "Alpha")
	}
	if out[1].Text != "bravo charlie delta" || out[1].StartMS < 4000 {
		t.Errorf("second run cue = %+v, want text %q starting at or after 4000", out[1], "bravo charlie delta")
	}
}

// TestReflowTranslatedCuesEveryRunGetsAToken pins the rounding rule: one
// source word then nine, with three translated tokens. Proportion alone gives
// the first run 0 tokens; the rule gives it 1 while tokens remain.
func TestReflowTranslatedCuesEveryRunGetsAToken(t *testing.T) {
	words := []model.WordSpan{{T: 0, D: 300, W: "w0"}}
	for i := 0; i < 9; i++ {
		words = append(words, model.WordSpan{T: 5000 + i*300, D: 250, W: "w"})
	}
	src := []subtitle.TranscriptChunk{{Text: "x", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 7650, Words: words}}}
	tr := []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 7650, Text: "Eins zwei drei"}}
	out := subtitle.ReflowTranslatedCues(tr, src, 2000)
	if len(out) != 2 {
		t.Fatalf("want 2 cues, got %d: %+v", len(out), out)
	}
	if out[0].Text != "Eins" || out[1].Text != "zwei drei" {
		t.Fatalf("token shares = %q / %q, want %q / %q", out[0].Text, out[1].Text, "Eins", "zwei drei")
	}
}

// TestReflowTranslatedCuesOffMatchesReflow pins that source_pause_ms 0 turns
// the rule off: the output equals ReflowChunkCues byte for byte.
func TestReflowTranslatedCuesOffMatchesReflow(t *testing.T) {
	want := subtitle.ReflowChunkCues(pausedTranslation())
	if len(want) == 0 {
		t.Fatal("baseline ReflowChunkCues returned no cues")
	}
	got := subtitle.ReflowTranslatedCues(pausedTranslation(), pausedSource(), 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source_pause_ms 0 changed the output:\n got %+v\nwant %+v", got, want)
	}
}

// TestReflowTranslatedCuesNoSourceWordsMatchesReflow pins that a source
// without word timings (and no source at all) gives today's output.
func TestReflowTranslatedCuesNoSourceWordsMatchesReflow(t *testing.T) {
	want := subtitle.ReflowChunkCues(pausedTranslation())
	if len(want) == 0 {
		t.Fatal("baseline ReflowChunkCues returned no cues")
	}
	bare := pausedSource()
	for i := range bare {
		bare[i].Span.Words = nil
	}
	if got := subtitle.ReflowTranslatedCues(pausedTranslation(), bare, 2000); !reflect.DeepEqual(got, want) {
		t.Fatalf("a source without word timings changed the output:\n got %+v\nwant %+v", got, want)
	}
	if got := subtitle.ReflowTranslatedCues(pausedTranslation(), nil, 2000); !reflect.DeepEqual(got, want) {
		t.Fatalf("no source changed the output:\n got %+v\nwant %+v", got, want)
	}
}

// TestReflowTranslatedCuesShortPauseKeepsSpreading pins that a source segment
// with no pause of at least source_pause_ms keeps the per-segment spreading.
func TestReflowTranslatedCuesShortPauseKeepsSpreading(t *testing.T) {
	want := subtitle.ReflowChunkCues(pausedTranslation())
	got := subtitle.ReflowTranslatedCues(pausedTranslation(), pausedSource(), 6000)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("a 5.1 s gap under a 6 s threshold changed the output:\n got %+v\nwant %+v", got, want)
	}
}

// TestReflowTranslatedCuesRelaxStopsAtNextRun pins the relaxation bound: a
// dense cue may extend into the pause after it, but never past the start of
// the next spoken run, even when that run gets no translated token and the
// next cue is far later.
func TestReflowTranslatedCuesRelaxStopsAtNextRun(t *testing.T) {
	src := []subtitle.TranscriptChunk{
		{Text: "a b c", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 6300, Words: []model.WordSpan{
			{T: 0, D: 300, W: "a"}, {T: 3000, D: 300, W: "b"}, {T: 6000, D: 300, W: "c"},
		}}},
		{Text: "d", Span: model.Span{Kind: "time", StartMS: 20000, EndMS: 20400, Words: []model.WordSpan{{T: 20000, D: 400, W: "d"}}}},
	}
	long := strings.Repeat("x", 60)
	tr := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 6300, Text: long},
		{Index: 2, StartMS: 20000, EndMS: 20400, Text: "fin"},
	}
	out := subtitle.ReflowTranslatedCues(tr, src, 2000)
	first := cueWith(t, out, long)
	if first.EndMS <= 300 {
		t.Fatalf("dense cue %+v was not relaxed into the pause after it", first)
	}
	if first.EndMS > 3000 {
		t.Fatalf("dense cue %+v runs past the next spoken run (3000)", first)
	}
	// Control: with the rule off the same cue runs well past 3000.
	if off := cueWith(t, subtitle.ReflowTranslatedCues(tr, src, 0), long); off.EndMS <= 3000 {
		t.Fatalf("fixture is too weak: with the rule off the cue %+v already ends by 3000", off)
	}
}

// TestReflowTranslatedCuesNoLeadInToPause pins that a cue which opens a spoken
// run after a source pause does not start inside that pause, even when it is
// dense and the next cue leaves it no room to grow at its end.
func TestReflowTranslatedCuesNoLeadInToPause(t *testing.T) {
	src := []subtitle.TranscriptChunk{
		{Text: "a b", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 3300, Words: []model.WordSpan{
			{T: 0, D: 300, W: "a"}, {T: 3000, D: 300, W: "b"},
		}}},
		{Text: "c", Span: model.Span{Kind: "time", StartMS: 4000, EndMS: 4400, Words: []model.WordSpan{{T: 4000, D: 400, W: "c"}}}},
	}
	long := strings.Repeat("y", 60)
	tr := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 3300, Text: "Hi " + long},
		{Index: 2, StartMS: 4000, EndMS: 4400, Text: "fin"},
	}
	out := subtitle.ReflowTranslatedCues(tr, src, 2000)
	if hi := cueWith(t, out, "Hi"); hi.EndMS > 3000 {
		t.Fatalf("first-run cue %+v runs past the next spoken run (3000)", hi)
	}
	if c := cueWith(t, out, long); c.StartMS < 3000 {
		t.Fatalf("cue %+v opens the second run but starts in the pause before 3000", c)
	}
}
