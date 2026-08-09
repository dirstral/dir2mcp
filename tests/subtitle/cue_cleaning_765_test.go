package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// Issue #765 split the drop_urls verdict and the collapse_repeats run counter out
// of CleanCues so the ingest pass can make the SAME verdicts on chunk segments
// before embedding. These tests pin the extracted primitives directly, because a
// drift between them and CleanCues is exactly the failure mode the split exists
// to prevent (two copies of a pattern, two answers, index and sidecar disagree).

// TestIsURLCueMatchesHallucinatedCredits pins the shared URL/credit-line verdict:
// it catches real domains in any position without hardcoding ccTLDs, and leaves
// ordinary prose with sentence periods, decimals and honorifics alone.
func TestIsURLCueMatchesHallucinatedCredits(t *testing.T) {
	urls := []string{
		"https://example.com",
		"Subtitles by www.example.org",
		"amara.org",
		"Субтитры сделал DimaTorzok tvrain.tv",
	}
	for _, s := range urls {
		if !subtitle.IsURLCue(s) {
			t.Errorf("IsURLCue(%q) = false, want true", s)
		}
	}
	prose := []string{
		"That is the end. Next we turn to the economy.",
		"Inflation reached 3.14 percent",
		"Mr. Smith took the floor",
		"Сегодня обсуждаем экономику региона",
	}
	for _, s := range prose {
		if subtitle.IsURLCue(s) {
			t.Errorf("IsURLCue(%q) = true, want false", s)
		}
	}
}

// TestRepeatCollapserKeepsLeadingRun pins the run counter: a run of K identical
// texts keeps the first limit-1 and drops the rest, a changed text restarts the
// run, and a limit below 2 is inactive (legitimate short repeats survive).
func TestRepeatCollapserKeepsLeadingRun(t *testing.T) {
	c := subtitle.NewRepeatCollapser(3)
	if !c.Active() {
		t.Fatal("collapser with limit 3 must be active")
	}
	seq := []string{"a", "a", "a", "a", "b", "a"}
	want := []bool{false, false, true, true, false, false}
	for i, text := range seq {
		if got := c.Drop(text); got != want[i] {
			t.Fatalf("Drop(%q) at %d = %v, want %v", text, i, got, want[i])
		}
	}

	for _, limit := range []int{0, 1, -1} {
		inactive := subtitle.NewRepeatCollapser(limit)
		if inactive.Active() {
			t.Fatalf("collapser with limit %d must be inactive", limit)
		}
		for _, text := range seq {
			if inactive.Drop(text) {
				t.Fatalf("inactive collapser (limit %d) dropped %q", limit, text)
			}
		}
	}
	// A nil collapser is inactive too, so callers need no nil guard.
	var nilCollapser *subtitle.RepeatCollapser
	if nilCollapser.Active() || nilCollapser.Drop("a") {
		t.Fatal("nil collapser must be inactive")
	}
}

// TestCleanCuesUsesSharedRepeatCounter pins that CleanCues still collapses runs
// exactly as before the primitives were extracted: the first limit-1 cues of a
// run survive, and surviving cues are re-indexed gap-free.
func TestCleanCuesUsesSharedRepeatCounter(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "opening"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "repeat"},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "repeat"},
		{Index: 4, StartMS: 3000, EndMS: 4000, Text: "repeat"},
		{Index: 5, StartMS: 4000, EndMS: 5000, Text: "closing"},
	}
	got := subtitle.CleanCues(cues, subtitle.CleanOptions{CollapseRepeats: 2})
	want := []string{"opening", "repeat", "closing"}
	if len(got) != len(want) {
		t.Fatalf("CleanCues kept %d cues, want %d: %+v", len(got), len(want), got)
	}
	for i, c := range got {
		if c.Text != want[i] {
			t.Fatalf("cue %d = %q, want %q", i, c.Text, want[i])
		}
		if c.Index != i+1 {
			t.Fatalf("cue %d has index %d, want gap-free %d", i, c.Index, i+1)
		}
	}
}
