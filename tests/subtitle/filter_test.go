package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestWordFilterInactiveIdentity pins that an empty/whitespace-only phrase list
// yields an inactive filter whose Apply is the identity, so the empty-config
// path is a no-op everywhere.
func TestWordFilterInactiveIdentity(t *testing.T) {
	for _, phrases := range [][]string{nil, {}, {"", "   "}} {
		f := subtitle.NewWordFilter(phrases)
		if f.Active() {
			t.Fatalf("filter %v: Active()=true, want false", phrases)
		}
		const in = "Subscribe to TV Rain for more"
		if got := f.Apply(in); got != in {
			t.Fatalf("inactive Apply changed text: got %q want %q", got, in)
		}
	}
}

// TestWordFilterCaseInsensitiveSubstring pins case-insensitive substring
// removal: every matched occurrence (regardless of casing) is stripped while the
// surrounding text keeps its original casing.
func TestWordFilterCaseInsensitiveSubstring(t *testing.T) {
	f := subtitle.NewWordFilter([]string{"watermark"})
	got := f.Apply("Keep WATERMARK this WaterMark text")
	want := "Keep  this  text"
	// Apply trims outer whitespace but not the interior double-spaces left by
	// removal; assert the phrase is gone and casing of survivors is preserved.
	if got != want {
		t.Fatalf("case-insensitive removal: got %q want %q", got, want)
	}
}

// TestWordFilterEmptyAfterFilter pins that a text consisting solely of (a)
// filter phrase(s) becomes empty after Apply, which callers treat as "drop".
func TestWordFilterEmptyAfterFilter(t *testing.T) {
	f := subtitle.NewWordFilter([]string{"credits", "the end"})
	if got := f.Apply("  Credits  "); got != "" {
		t.Fatalf("phrase-only text should be empty after filter, got %q", got)
	}
	if got := f.Apply("THE END credits"); got != "" {
		t.Fatalf("multi-phrase-only text should be empty after filter, got %q", got)
	}
}

// TestWordFilterMultiPhraseOrder pins that multiple phrases are all applied and
// that removing one phrase can expose another (deterministic, config-order).
func TestWordFilterMultiPhraseOrder(t *testing.T) {
	f := subtitle.NewWordFilter([]string{"ab", "c"})
	// Both phrases are applied (case-insensitively): "xABcy" loses "AB" and "c".
	got := f.Apply("xABcy")
	if got != "xy" {
		t.Fatalf("multi-phrase removal: got %q want %q", got, "xy")
	}
}

// TestFilterCusDropsAndReindexes pins FilterCues: cues empty after filtering are
// dropped and survivors are contiguously re-indexed; timing is preserved.
func TestFilterCuesDropsAndReindexes(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Hello world"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "Subscribe now"},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "Goodbye Subscribe now"},
	}
	f := subtitle.NewWordFilter([]string{"subscribe now"})
	got := subtitle.FilterCues(cues, f)
	if len(got) != 2 {
		t.Fatalf("FilterCues len = %d, want 2 (one cue dropped): %#v", len(got), got)
	}
	if got[0].Index != 1 || got[0].Text != "Hello world" || got[0].StartMS != 0 || got[0].EndMS != 1000 {
		t.Fatalf("cue[0] = %#v, want untouched Hello world", got[0])
	}
	if got[1].Index != 2 || got[1].Text != "Goodbye" || got[1].StartMS != 2000 || got[1].EndMS != 3000 {
		t.Fatalf("cue[1] = %#v, want re-indexed filtered Goodbye with original timing", got[1])
	}
}

// TestFilterCuesInactiveIdentity pins that an inactive filter returns the cues
// unchanged (no-op), so the empty-config export path is byte-identical.
func TestFilterCuesInactiveIdentity(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Subscribe now"},
	}
	got := subtitle.FilterCues(cues, subtitle.NewWordFilter(nil))
	if len(got) != 1 || got[0].Text != "Subscribe now" {
		t.Fatalf("inactive FilterCues changed cues: %#v", got)
	}
}
