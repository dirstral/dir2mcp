package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func TestWhisperTranslateOverlapMS(t *testing.T) {
	cases := []struct{ window, want int }{
		{45000, 9000},   // one fifth
		{100000, 10000}, // capped
		{1000, 200},
		{0, 0},
	}
	for _, c := range cases {
		if got := ingest.WhisperTranslateOverlapMS(c.window); got != c.want {
			t.Errorf("WhisperTranslateOverlapMS(%d) = %d, want %d", c.window, got, c.want)
		}
	}
}

// TestMergeTranslateWindows pins the offset + core-dedup: each window's local
// timestamps are shifted by its start, and text/words in the overlap are kept
// from exactly one window (the one whose core they fall in), except the final
// window which keeps everything through the end.
func TestMergeTranslateWindows(t *testing.T) {
	windows := []ingest.TranslateWindow{
		{StartMS: 0, Res: model.TranscriptResult{
			Text: "[0:00] a\n[0:03] b\n[0:04] c", // "c" at abs 4000 is in the overlap
			Words: []model.TimedWord{
				{Word: "a", StartMS: 0, EndMS: 500},
				{Word: "b", StartMS: 3000, EndMS: 3500},
				{Word: "c", StartMS: 4500, EndMS: 5000}, // abs 4500 -> outside core, dropped
			},
		}},
		{StartMS: 4000, Res: model.TranscriptResult{
			Text: "[0:00] c\n[0:02] d", // "c" re-decoded here (abs 4000), "d" at abs 6000
			Words: []model.TimedWord{
				{Word: "c", StartMS: 500, EndMS: 1000},  // abs 4500
				{Word: "d", StartMS: 2000, EndMS: 2500}, // abs 6000
			},
		}},
	}
	text, words := ingest.MergeTranslateWindows(windows, 4000)

	// Words: a@0, b@3000 (window 0 core), then c@4500, d@6000 (window 1, the last,
	// keeps all). The window-0 "c" at 4500 was dropped as the overlap duplicate.
	wantWords := []model.TimedWord{
		{Word: "a", StartMS: 0, EndMS: 500},
		{Word: "b", StartMS: 3000, EndMS: 3500},
		{Word: "c", StartMS: 4500, EndMS: 5000},
		{Word: "d", StartMS: 6000, EndMS: 6500},
	}
	if len(words) != len(wantWords) {
		t.Fatalf("word count = %d, want %d: %+v", len(words), len(wantWords), words)
	}
	for i, w := range wantWords {
		if words[i] != w {
			t.Errorf("word[%d] = %+v, want %+v", i, words[i], w)
		}
	}

	// Text: "c" appears exactly once (from window 1, at abs 00:04), no duplicate.
	want := "[00:00] a\n[00:03] b\n[00:04] c\n[00:06] d"
	if text != want {
		t.Errorf("merged text =\n%q\nwant\n%q", text, want)
	}
}

// TestMergeTranslateWindowsSingle pins that a single window is a straight offset
// (no dedup drops anything, since it is the final window).
func TestMergeTranslateWindowsSingle(t *testing.T) {
	windows := []ingest.TranslateWindow{
		{StartMS: 2000, Res: model.TranscriptResult{
			Text:  "[0:01] hello",
			Words: []model.TimedWord{{Word: "hello", StartMS: 1000, EndMS: 1500}},
		}},
	}
	text, words := ingest.MergeTranslateWindows(windows, 4000)
	if len(words) != 1 || words[0].StartMS != 3000 {
		t.Fatalf("expected one word offset to 3000ms, got %+v", words)
	}
	if text != "[00:03] hello" {
		t.Errorf("text = %q, want %q", text, "[00:03] hello")
	}
}

// TestMergeTranslateWindowsDedupsStraddlingSentence pins the fuzzy overlap dedup:
// when overlapping windows time the SAME sentence slightly differently so its
// start straddles the core boundary, core-boundary dedup keeps both copies — the
// word-overlap pass must collapse them to one, keeping the fuller wording and its
// words (so the transcript never doubles a sentence, and word-timed segmentation
// downstream doesn't interleave the duplicate into word-salad).
func TestMergeTranslateWindowsDedupsStraddlingSentence(t *testing.T) {
	mk := func(local int, w string) model.TimedWord {
		return model.TimedWord{Word: w, StartMS: local, EndMS: local + 150}
	}
	windows := []ingest.TranslateWindow{
		{StartMS: 0, Res: model.TranscriptResult{ // decodes the sentence near its end (abs 3000)
			Text:  "[0:03] I will be very happy",
			Words: []model.TimedWord{mk(3000, "I"), mk(3200, "will"), mk(3400, "be"), mk(3600, "very"), mk(3800, "happy")},
		}},
		{StartMS: 4000, Res: model.TranscriptResult{ // re-decodes it fuller near its start (abs 4000)
			Text:  "[0:00] I will be very happy to talk",
			Words: []model.TimedWord{mk(0, "I"), mk(200, "will"), mk(400, "be"), mk(600, "very"), mk(800, "happy"), mk(1000, "to"), mk(1200, "talk")},
		}},
	}
	text, words := ingest.MergeTranslateWindows(windows, 4000)
	if n := strings.Count(text, "very happy"); n != 1 {
		t.Errorf("sentence doubled: %q (want one 'very happy', got %d)", text, n)
	}
	if strings.Count(text, "\n") != 0 {
		t.Errorf("expected a single merged segment, got:\n%s", text)
	}
	// The fuller decode's words are kept (7, ending in "talk"), not the shorter 5.
	if len(words) != 7 {
		t.Errorf("expected 7 words from the fuller decode, got %d: %+v", len(words), words)
	}
}

// TestMergeTranslateWindowsKeepsDistinctSegments guards against over-merging:
// two low-overlap segments close in time must both survive.
func TestMergeTranslateWindowsKeepsDistinctSegments(t *testing.T) {
	windows := []ingest.TranslateWindow{
		{StartMS: 0, Res: model.TranscriptResult{
			Text: "[0:00] the harvest was good this autumn\n[0:03] but the winter came early",
		}},
	}
	text, _ := ingest.MergeTranslateWindows(windows, 8000)
	if strings.Count(text, "\n") != 1 {
		t.Errorf("distinct segments were merged; want 2 lines, got:\n%s", text)
	}
}

// TestMergeTranslateWindowsMergesMistimedSegments pins the span-adequacy guard:
// a segment carrying seconds of text but whose next segment starts implausibly
// soon (a window-boundary timing artifact) is merged forward so the combined text
// gets the combined span — otherwise a segment-timed export would crush it.
func TestMergeTranslateWindowsMergesMistimedSegments(t *testing.T) {
	// Three ~45-char segments only 1 s apart (at 17 cps a 45-char cue needs ~2.6 s,
	// so 1 s of span is far too tight) must merge into one group whose span reaches
	// the readable segment 12 s out, which stays separate.
	windows := []ingest.TranslateWindow{
		{StartMS: 0, Res: model.TranscriptResult{Text: strings.Join([]string{
			"[0:00] there were many different kinds of hearings",
			"[0:01] on the release of the prisoners that year",
			"[0:02] and on the service the people had rendered",
			"[0:12] then he asked me a very different question",
		}, "\n")}},
	}
	text, _ := ingest.MergeTranslateWindows(windows, 20000)
	lines := strings.Split(text, "\n")
	// The three crammed segments collapse into one group; the distant one stays.
	if len(lines) != 2 {
		t.Fatalf("expected the 3 crammed segments to merge into 1 (+1 distant) = 2 lines, got %d:\n%s", len(lines), text)
	}
	if !strings.Contains(lines[0], "hearings") || !strings.Contains(lines[0], "rendered") {
		t.Errorf("merged group missing text: %q", lines[0])
	}
	if !strings.Contains(lines[1], "different question") {
		t.Errorf("distant segment lost: %q", lines[1])
	}
}

// TestTranslateWindowStarts_DropsTooShortFinalWindow pins the fix for the
// tiny-final-window failure: a trailing window shorter than the overlap is
// dropped (it would be a sub-second decode that ffmpeg errors on / Whisper
// hallucinates), while a healthy tail and the single-window / exact-multiple
// cases are preserved.
func TestTranslateWindowStarts_DropsTooShortFinalWindow(t *testing.T) {
	const step, overlap = 100_000, 20_000 // 100s step, 20s overlap (120s window)
	for _, tc := range []struct {
		name    string
		totalMS int
		want    []int
	}{
		{"exact multiple keeps all", 300_000, []int{0, 100_000, 200_000}},
		{"healthy tail kept", 340_000, []int{0, 100_000, 200_000, 300_000}},
		{"tiny tail dropped", 305_000, []int{0, 100_000, 200_000}},
		{"tail just under overlap dropped", 319_999, []int{0, 100_000, 200_000}},
		{"tail exactly overlap kept", 320_000, []int{0, 100_000, 200_000, 300_000}},
		{"single window untouched", 80_000, []int{0}},
		{"sub-step audio untouched", 5_000, []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.TranslateWindowStarts(tc.totalMS, step, overlap)
			if len(got) != len(tc.want) {
				t.Fatalf("starts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("starts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
