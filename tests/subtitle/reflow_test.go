package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// cueText joins the tokens of every cue in reading order, so a test can assert
// that reflow preserved the words (it re-segments, so exact cue boundaries and
// wrap newlines differ, but no word is lost or reordered).
func cueText(cues []subtitle.Cue) string {
	var b []string
	for _, c := range cues {
		b = append(b, strings.Fields(strings.ReplaceAll(c.Text, "\n", " "))...)
	}
	return strings.Join(b, " ")
}

// maxCPS returns the highest characters-per-second reading speed over all cues.
func maxCPS(cues []subtitle.Cue) float64 {
	worst := 0.0
	for _, c := range cues {
		dur := float64(c.EndMS-c.StartMS) / 1000.0
		if dur <= 0 {
			continue
		}
		cps := float64(len([]rune(strings.ReplaceAll(c.Text, "\n", "")))) / dur
		if cps > worst {
			worst = cps
		}
	}
	return worst
}

// TestReflowChunkCuesEmpty pins that reflow of no cues (or blank cues) returns
// the input unchanged rather than panicking on the empty run.
func TestReflowChunkCuesEmpty(t *testing.T) {
	if got := subtitle.ReflowChunkCues(nil); got != nil {
		t.Fatalf("ReflowChunkCues(nil) = %v, want nil", got)
	}
	blank := []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 1000, Text: "   "}}
	got := subtitle.ReflowChunkCues(blank)
	if len(got) != 1 || strings.TrimSpace(got[0].Text) != "" {
		t.Fatalf("ReflowChunkCues(blank) = %+v, want the blank cue back", got)
	}
}

// TestReflowChunkCuesEvensReadingSpeed pins the core behaviour: a dense source
// cue (a long clause crammed into a sub-second slot) that sits in the same
// contiguous run as a sparse neighbour borrows time from it, so no output cue
// reads far faster than the run average. The run here is ~90 chars over 6 s
// (~15 cps), so every cue must land well under a 22 cps legibility ceiling —
// which the dense input cue alone (~55 chars in 0.6 s ≈ 90 cps) badly violated.
func TestReflowChunkCuesEvensReadingSpeed(t *testing.T) {
	in := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 600, Text: "We have submitted a formal request to the ministry today."},
		{Index: 2, StartMS: 600, EndMS: 6000, Text: "Yes."},
	}
	out := subtitle.ReflowChunkCues(in)
	if len(out) == 0 {
		t.Fatal("ReflowChunkCues returned no cues")
	}
	if got := maxCPS(out); got > 22 {
		t.Errorf("max reading speed %.1f cps exceeds 22 after reflow; cues=%+v", got, out)
	}
	if before := maxCPS(in); before <= 22 {
		t.Fatalf("test precondition: input should be dense (>22 cps), got %.1f", before)
	}
}

// TestReflowChunkCuesPreservesText pins that reflow neither drops nor reorders
// words: the concatenated token stream is identical before and after.
func TestReflowChunkCuesPreservesText(t *testing.T) {
	in := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 900, Text: "The first sentence is here."},
		{Index: 2, StartMS: 900, EndMS: 1800, Text: "A second one follows it closely."},
		{Index: 3, StartMS: 1800, EndMS: 4000, Text: "And then a third, longer and calmer, closes the thought."},
	}
	out := subtitle.ReflowChunkCues(in)
	if want, got := cueText(in), cueText(out); want != got {
		t.Errorf("reflow changed text:\n want %q\n got  %q", want, got)
	}
}

// TestReflowChunkCuesValidTimings pins that reflow output is always renderable:
// positive, <= 6 s durations, <= 42-char lines, and no overlaps between cues.
func TestReflowChunkCuesValidTimings(t *testing.T) {
	in := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 500, Text: "A very long and dense sentence that will not fit its short original slot at all."},
		{Index: 2, StartMS: 500, EndMS: 1200, Text: "More words keep coming without any pause to breathe here."},
		{Index: 3, StartMS: 1200, EndMS: 9000, Text: "Then silence."},
	}
	out := subtitle.ReflowChunkCues(in)
	for i, c := range out {
		if c.EndMS <= c.StartMS {
			t.Errorf("cue %d has non-positive duration (%d -> %d)", i, c.StartMS, c.EndMS)
		}
		if c.EndMS-c.StartMS > 6000 {
			t.Errorf("cue %d duration %d ms exceeds 6000 ms cap", i, c.EndMS-c.StartMS)
		}
		for _, line := range strings.Split(c.Text, "\n") {
			if n := len([]rune(line)); n > 42 {
				t.Errorf("cue %d line %q is %d chars, exceeds 42", i, line, n)
			}
		}
		if i > 0 && out[i-1].EndMS > c.StartMS {
			t.Errorf("cue %d overlaps previous (%d > %d)", i, out[i-1].EndMS, c.StartMS)
		}
	}
}

// TestReflowChunkCuesKeepsGaps pins that a silence gap wider than the pause
// threshold is a hard run boundary: text on either side keeps its own time
// budget and is not smeared across the gap, so the second run still starts only
// after the silence (a real pause the viewer sees).
func TestReflowChunkCuesKeepsGaps(t *testing.T) {
	in := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1500, Text: "First he spoke about the harvest."},
		// ~8 s of silence -> a hard run boundary, not a borrowable in-run gap.
		{Index: 2, StartMS: 9500, EndMS: 11000, Text: "Then, much later, about the winter."},
	}
	out := subtitle.ReflowChunkCues(in)
	if len(out) < 2 {
		t.Fatalf("expected the gap to keep the two runs separate, got %d cue(s): %+v", len(out), out)
	}
	// No cue may straddle the silence: every cue lies wholly before or wholly
	// after the gap midpoint (~5 s).
	for _, c := range out {
		if c.StartMS < 5000 && c.EndMS > 5000 {
			t.Errorf("cue %d straddles the silence gap (%d -> %d)", c.Index, c.StartMS, c.EndMS)
		}
	}
	if out[len(out)-1].StartMS < 5000 {
		t.Errorf("second run did not start after the silence: last cue starts at %d", out[len(out)-1].StartMS)
	}
}
