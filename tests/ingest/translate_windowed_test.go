package tests

import (
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
