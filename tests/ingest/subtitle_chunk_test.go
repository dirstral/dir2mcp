package tests

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestChunkSubtitleCues_BudgetsJoinSeparators guards CodeRabbit finding
// represent.go ~1053: chunkSubtitleCues merges cues with "\n" joins, so the
// separators must be counted against TranscriptChunkMaxChars. Cues whose texts
// plus their join separators sum to just over the cap must split, and NO emitted
// chunk may exceed TranscriptChunkMaxChars (before this fix the unbudgeted
// newlines pushed a merged chunk over the cap).
func TestChunkSubtitleCues_BudgetsJoinSeparators(t *testing.T) {
	t.Parallel()

	const max = ingest.TranscriptChunkMaxChars
	// Two equal cues each just under half the cap so their texts alone fit
	// (2*cueLen <= max) but with the joining "\n" they overflow
	// (2*cueLen + 1 > max), forcing a split that only the separator budget
	// detects.
	cueLen := max/2 + 1 // 2*cueLen = max+2 > max; with sep = max+3
	cueText := strings.Repeat("a", cueLen)

	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: cueText},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: cueText},
	}

	segs := ingest.ChunkSubtitleCues(cues)
	if len(segs) != 2 {
		t.Fatalf("expected the two cues to split into 2 chunks (separator budgeted), got %d", len(segs))
	}
	for i, s := range segs {
		if n := utf8.RuneCountInString(s.Text); n > max {
			t.Fatalf("chunk %d exceeds TranscriptChunkMaxChars: %d > %d", i, n, max)
		}
	}
}

// TestChunkSubtitleCues_MergesWithinBudget confirms the separator budget does
// not over-split: two cues that fit together (texts + the joining newline stay
// at or under the cap) still merge into one chunk spanning both.
func TestChunkSubtitleCues_MergesWithinBudget(t *testing.T) {
	t.Parallel()

	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "hello"},
		{Index: 2, StartMS: 1000, EndMS: 2500, Text: "world"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	if len(segs) != 1 {
		t.Fatalf("expected the two small cues to merge into 1 chunk, got %d", len(segs))
	}
	if segs[0].Text != "hello\nworld" {
		t.Fatalf("expected merged text %q, got %q", "hello\nworld", segs[0].Text)
	}
	if segs[0].Span.StartMS != 0 || segs[0].Span.EndMS != 2500 {
		t.Fatalf("expected merged span [0,2500], got [%d,%d]", segs[0].Span.StartMS, segs[0].Span.EndMS)
	}
}
