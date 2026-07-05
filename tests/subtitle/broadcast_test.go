package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// word is a compact (start,end,text) triple for building word-timed test spans.
type word struct {
	start int
	end   int
	text  string
}

// timeChunkWithWords wraps a word list into a single "time" TranscriptChunk. The
// chunk Text is unused by BuildBroadcastCues (it rebuilds text from words), so it
// is left blank to prove the builder reads only the word timings.
func timeChunkWithWords(words []word) subtitle.TranscriptChunk {
	ws := make([]model.WordSpan, 0, len(words))
	for _, w := range words {
		ws = append(ws, model.WordSpan{T: w.start, D: w.end - w.start, W: w.text})
	}
	start, end := 0, 0
	if len(words) > 0 {
		start, end = words[0].start, words[len(words)-1].end
	}
	return subtitle.TranscriptChunk{
		Span: model.Span{Kind: "time", StartMS: start, EndMS: end, Words: ws},
	}
}

// TestBuildBroadcastCuesEmpty pins that a transcript with no word timings yields
// nil, so the export path falls back to BuildCues (chunk-per-cue) unchanged.
func TestBuildBroadcastCuesEmpty(t *testing.T) {
	// A time span with no words, and a non-time span with words that must be ignored.
	chunks := []subtitle.TranscriptChunk{
		{Text: "no words", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1000}},
		{Text: "x", Span: model.Span{Kind: "text", Words: []model.WordSpan{{T: 0, D: 100, W: "ignored"}}}},
	}
	if got := subtitle.BuildBroadcastCues(chunks); got != nil {
		t.Fatalf("BuildBroadcastCues with no time-span words = %v, want nil", got)
	}
}

// TestBuildBroadcastCuesMaxDuration pins the 6 s hard cap: a steady stream of
// short words with no pause and no sentence end must still break so no cue spans
// more than 6 s.
func TestBuildBroadcastCuesMaxDuration(t *testing.T) {
	// 20 words, 500 ms apart, contiguous (no gaps) -> only the duration cap can break.
	var words []word
	for i := 0; i < 20; i++ {
		words = append(words, word{start: i * 500, end: i*500 + 500, text: "w"})
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) < 2 {
		t.Fatalf("expected multiple cues from a 10 s run, got %d", len(cues))
	}
	for _, c := range cues {
		if dur := c.EndMS - c.StartMS; dur > 6000 {
			t.Errorf("cue %d duration %d ms exceeds 6000 ms cap", c.Index, dur)
		}
		if c.EndMS < c.StartMS {
			t.Errorf("cue %d has negative duration (%d -> %d)", c.Index, c.StartMS, c.EndMS)
		}
	}
}

// TestBuildBroadcastCuesPauseBreak pins that a speech pause (> 600 ms) after a
// cue that already meets the minimum on-screen time ends the cue there.
func TestBuildBroadcastCuesPauseBreak(t *testing.T) {
	words := []word{
		{start: 0, end: 400, text: "Hello"},
		{start: 400, end: 1400, text: "there"}, // cur dur now 1.4 s (>= 1.2 s min)
		// 1 s gap -> pause break before "friend"
		{start: 2400, end: 2900, text: "friend"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues split at the pause, got %d: %+v", len(cues), cues)
	}
	if !strings.Contains(cues[0].Text, "Hello there") {
		t.Errorf("cue 0 text = %q, want to contain %q", cues[0].Text, "Hello there")
	}
	if !strings.Contains(cues[1].Text, "friend") {
		t.Errorf("cue 1 text = %q, want to contain %q", cues[1].Text, "friend")
	}
}

// TestBuildBroadcastCuesMinDuration pins that a very short utterance is extended
// to the minimum on-screen time by borrowing from the following silence, without
// overlapping the next cue.
func TestBuildBroadcastCuesMinDuration(t *testing.T) {
	words := []word{
		{start: 0, end: 200, text: "Da."}, // 200 ms spoken, sentence end
		// long gap, then a well-separated second utterance far in the future
		{start: 10000, end: 10400, text: "Later"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d: %+v", len(cues), cues)
	}
	if dur := cues[0].EndMS - cues[0].StartMS; dur < 1200 {
		t.Errorf("short cue not extended to min: dur=%d ms, want >= 1200", dur)
	}
	if cues[0].EndMS >= cues[1].StartMS {
		t.Errorf("extended cue 0 overlaps cue 1: %d >= %d", cues[0].EndMS, cues[1].StartMS)
	}
}

// TestBuildBroadcastCuesOverlappingTimingsNeverTruncate pins that overlapping
// ASR word timings (a later cue's start preceding the current cue's spoken end)
// never cause the min-duration extension to pull a cue's end before its spoken
// end or produce a non-positive duration.
func TestBuildBroadcastCuesOverlappingTimingsNeverTruncate(t *testing.T) {
	// Two short sentence-final utterances whose word windows overlap: the second
	// word starts (900) before the first ends (1000). The first cue is short
	// enough to trigger extension, but the tight/overlapping neighbour leaves no
	// room — it must keep its spoken end, not truncate.
	words := []word{
		{start: 0, end: 1000, text: "Da."},
		{start: 900, end: 1050, text: "Net."},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	for _, c := range cues {
		if c.EndMS <= c.StartMS {
			t.Errorf("cue %d has non-positive duration (%d -> %d)", c.Index, c.StartMS, c.EndMS)
		}
	}
	// The first cue must not end before its own spoken end (1000 ms).
	if len(cues) > 0 && cues[0].EndMS < 1000 {
		t.Errorf("cue 0 end %d truncated below spoken end 1000", cues[0].EndMS)
	}
}

// TestBuildBroadcastCuesWrapAndDetok pins two-line balanced wrapping for a long
// cue and punctuation-flush detokenization of trimmed word tokens.
func TestBuildBroadcastCuesWrapAndDetok(t *testing.T) {
	// Nine ~7-char words + a comma token -> one cue over 42 chars, forcing a wrap.
	words := []word{
		{start: 0, end: 300, text: "alpha"},
		{start: 300, end: 600, text: "bravo"},
		{start: 600, end: 900, text: "charlie"},
		{start: 900, end: 1200, text: "delta"},
		{start: 1200, end: 1500, text: ","}, // standalone punctuation: no space before
		{start: 1500, end: 1800, text: "echo"},
		{start: 1800, end: 2100, text: "foxtrot"},
		{start: 2100, end: 2400, text: "golf"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d: %+v", len(cues), cues)
	}
	got := cues[0].Text
	if !strings.Contains(got, "delta,") {
		t.Errorf("detok: comma not flush to preceding word: %q", got)
	}
	if strings.Contains(got, "delta ,") {
		t.Errorf("detok: stray space before comma: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("expected a two-line wrap, got single line: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if n := len([]rune(line)); n > 42 {
			t.Errorf("wrapped line %q is %d chars, exceeds 42", line, n)
		}
	}
}

// TestBuildBroadcastCuesHyphenDetok pins that a hyphenated compound whisper emits
// as split trimmed tokens ("so", "-called") renders flush ("so-called"), in both
// Latin and Cyrillic, and never as "so -called". A leading dash after a
// non-alphanumeric is left spaced (not glued onto punctuation).
func TestBuildBroadcastCuesHyphenDetok(t *testing.T) {
	words := []word{
		{start: 0, end: 300, text: "the"},
		{start: 300, end: 600, text: "so"},
		{start: 600, end: 900, text: "-called"},
		{start: 900, end: 1200, text: "expert"},
		{start: 1200, end: 1500, text: "что"},
		{start: 1500, end: 1800, text: "-то"},
		{start: 1800, end: 2100, text: "said"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d: %+v", len(cues), cues)
	}
	got := strings.ReplaceAll(cues[0].Text, "\n", " ")
	for _, want := range []string{"so-called", "что-то"} {
		if !strings.Contains(got, want) {
			t.Errorf("hyphen detok: expected %q in %q", want, got)
		}
	}
	if strings.Contains(got, " -") {
		t.Errorf("hyphen detok: stray space before hyphen in %q", got)
	}
}

// TestBuildBroadcastCuesCapsRunawayDuration pins the display-duration cap: a
// single word whisper assigns a huge duration (e.g. 25 s over B-roll silence)
// must not produce a 25 s cue — display time is capped at 6 s.
func TestBuildBroadcastCuesCapsRunawayDuration(t *testing.T) {
	words := []word{
		{start: 0, end: 25000, text: "solo"}, // one word, 25 s spoken end
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d: %+v", len(cues), cues)
	}
	if dur := cues[0].EndMS - cues[0].StartMS; dur > 6000 {
		t.Errorf("runaway single-word cue duration %d ms exceeds 6000 ms cap", dur)
	}
	if cues[0].EndMS <= cues[0].StartMS {
		t.Errorf("cue has non-positive duration (%d -> %d)", cues[0].StartMS, cues[0].EndMS)
	}
}
