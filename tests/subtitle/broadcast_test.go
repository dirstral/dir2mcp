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

// speakerChunk wraps a word list into a single diarized "time" chunk carrying
// the given speaker label, for the speaker-boundary tests.
func speakerChunk(speaker string, words []word) subtitle.TranscriptChunk {
	c := timeChunkWithWords(words)
	c.Span.SpeakerLabel = speaker
	return c
}

// TestBuildBroadcastCuesBreaksOnSpeakerChange pins that a speaker change is a
// hard cue boundary and that each cue carries its speaker through to voice
// markup. The two speakers are contiguous in time with NO pause, so without a
// speaker-aware break their words (well under the char and duration caps) would
// merge into one cue — regressing BuildCues, which keeps one cue per span.
func TestBuildBroadcastCuesBreaksOnSpeakerChange(t *testing.T) {
	chunks := []subtitle.TranscriptChunk{
		speakerChunk("Alice", []word{{0, 400, "Hello"}, {400, 800, "there"}}),
		speakerChunk("Bob", []word{{800, 1200, "Hi"}, {1200, 1600, "back"}}),
	}
	cues := subtitle.BuildBroadcastCues(chunks)
	if len(cues) != 2 {
		t.Fatalf("expected a break at the speaker change, got %d: %+v", len(cues), cues)
	}
	if cues[0].Speaker != "Alice" || cues[1].Speaker != "Bob" {
		t.Errorf("speaker metadata lost: cue0=%q cue1=%q, want Alice/Bob", cues[0].Speaker, cues[1].Speaker)
	}
	if !strings.Contains(cues[0].Text, "Hello there") {
		t.Errorf("cue 0 text = %q, want Alice's words", cues[0].Text)
	}
	if !strings.Contains(cues[1].Text, "Hi back") {
		t.Errorf("cue 1 text = %q, want Bob's words", cues[1].Text)
	}
}

// TestBuildBroadcastCuesShortUtteranceBeforePauseBreaks pins that a natural
// break is NOT gated on spoken duration: a short but complete utterance before a
// real pause gets its own cue rather than merging across the pause. The minimum
// on-screen time is a display constraint bought later from silence by
// relaxBroadcastTiming, not a reason to withhold the segmentation break.
func TestBuildBroadcastCuesShortUtteranceBeforePauseBreaks(t *testing.T) {
	words := []word{
		{start: 0, end: 300, text: "Yes."}, // 300 ms spoken, far under the 1200 ms min
		// 700 ms gap (> 600 ms pause threshold) -> break despite the short duration.
		{start: 1000, end: 1500, text: "Continue"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 2 {
		t.Fatalf("short utterance before a pause must break into its own cue, got %d: %+v", len(cues), cues)
	}
	if strings.Contains(cues[0].Text, "Continue") {
		t.Errorf("cue 0 merged across the pause: %q", cues[0].Text)
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

// TestBuildBroadcastCuesSpacelessScriptFallsBack pins the CJK/spaceless-script
// safeguard: Whisper emits one token per character for scripts with no inter-word
// spaces (Chinese/Japanese/Thai), so the broadcast path — which rejoins tokens
// with an inserted space — must NOT run. BuildBroadcastCues returns nil so the
// caller falls back to BuildCues (verbatim chunk text), keeping "你好世界" intact
// rather than corrupting it into "你 好 世 界".
func TestBuildBroadcastCuesSpacelessScriptFallsBack(t *testing.T) {
	words := []word{
		{start: 0, end: 300, text: "你"},
		{start: 300, end: 600, text: "好"},
		{start: 600, end: 900, text: "世"},
		{start: 900, end: 1200, text: "界"},
	}
	if got := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)}); got != nil {
		t.Fatalf("BuildBroadcastCues on a spaceless (CJK) transcript = %v, want nil (fall back to chunk cues)", got)
	}

	// The caller's fallback (BuildCues over the stored chunk text) must reproduce
	// the text verbatim — no fabricated spaces between the ideographs.
	const cjk = "你好世界"
	chunks := []subtitle.TranscriptChunk{{Text: cjk, Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1200}}}
	cues := subtitle.BuildCues(chunks)
	if len(cues) != 1 {
		t.Fatalf("BuildCues fallback produced %d cues, want 1", len(cues))
	}
	if cues[0].Text != cjk {
		t.Errorf("fallback cue text = %q, want verbatim %q (no injected spaces)", cues[0].Text, cjk)
	}
}

// TestBuildBroadcastCuesCyrillicStillSegments pins that the spaceless-script
// guard does NOT misfire on a space-delimited script: a Cyrillic transcript is
// still re-segmented on the broadcast path (proving the detection is runes-not-
// bytes and the broadcast path is intact for non-Latin space-delimited scripts).
func TestBuildBroadcastCuesCyrillicStillSegments(t *testing.T) {
	words := []word{
		{start: 0, end: 400, text: "Привет"},
		{start: 400, end: 1400, text: "мир"}, // cur dur 1.4 s (>= 1.2 s min)
		// 1 s gap -> pause break before the next sentence
		{start: 2400, end: 2900, text: "друзья"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 2 {
		t.Fatalf("expected 2 Cyrillic cues split at the pause, got %d: %+v", len(cues), cues)
	}
	// The broadcast path space-joins Cyrillic tokens (correct for a space-delimited
	// script), so the first cue reads as spaced words.
	if !strings.Contains(cues[0].Text, "Привет мир") {
		t.Errorf("cue 0 text = %q, want to contain %q", cues[0].Text, "Привет мир")
	}
	if !strings.Contains(cues[1].Text, "друзья") {
		t.Errorf("cue 1 text = %q, want to contain %q", cues[1].Text, "друзья")
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
	for _, bad := range []string{"so -called", "что -то"} {
		if strings.Contains(got, bad) {
			t.Errorf("hyphen detok: unexpected spaced compound %q in %q", bad, got)
		}
	}
}

// TestBuildBroadcastCuesStandaloneDashNotGlued pins that a bare "-" token (a
// dialogue dash or numeric range, not a hyphenated-compound tail) stays spaced
// after a word — "word -" — and is never glued as "word-".
func TestBuildBroadcastCuesStandaloneDashNotGlued(t *testing.T) {
	words := []word{
		{start: 0, end: 300, text: "yes"},
		{start: 300, end: 600, text: "-"},
		{start: 600, end: 900, text: "no"},
	}
	cues := subtitle.BuildBroadcastCues([]subtitle.TranscriptChunk{timeChunkWithWords(words)})
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d: %+v", len(cues), cues)
	}
	got := strings.ReplaceAll(cues[0].Text, "\n", " ")
	if strings.Contains(got, "yes-") {
		t.Errorf("standalone dash glued onto preceding word in %q", got)
	}
	if !strings.Contains(got, "yes -") {
		t.Errorf("expected spaced standalone dash %q in %q", "yes -", got)
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
