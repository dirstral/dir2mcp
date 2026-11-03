package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// A chat-engine translation carries the source line's SPOKEN span (SPEC
// §8.6.2), not a span that runs to the next line's start. Otherwise the silence
// after a source cue becomes part of its translation and broadcast
// re-segmentation spreads the translated words across it, one word per cue,
// tens of seconds apart (RFE/RL feedback, 2026-11-03).
func TestTranslatedSegmentsInheritSpokenEnds(t *testing.T) {
	source := "[00:00] раз два\n[00:10] три\n[01:00] четыре"
	words := []model.TimedWord{
		{Word: "раз", StartMS: 0, EndMS: 500},
		{Word: "два", StartMS: 600, EndMS: 1200},
		{Word: "три", StartMS: 10000, EndMS: 10800},
		{Word: "четыре", StartMS: 60000, EndMS: 61000},
	}
	translated := "[00:00] one two\n[00:10] three\n[01:00] four"

	segs := ingest.ChunkTranslatedTranscriptByTime(translated, source, words)
	if len(segs) != 3 {
		t.Fatalf("segments: got %d, want 3", len(segs))
	}
	want := [][2]int{{0, 1200}, {10000, 10800}, {60000, 61000}}
	for i, w := range want {
		if segs[i].Span.StartMS != w[0] || segs[i].Span.EndMS != w[1] {
			t.Fatalf("segment %d span: got [%d,%d], want [%d,%d]", i, segs[i].Span.StartMS, segs[i].Span.EndMS, w[0], w[1])
		}
	}

	// No word timings: the historical rule is unchanged, a line runs to the
	// next line's start.
	plain := ingest.ChunkTranslatedTranscriptByTime(translated, source, nil)
	if len(plain) != 3 || plain[0].Span.EndMS != 10000 || plain[1].Span.EndMS != 60000 {
		t.Fatalf("without words: got %+v", plain)
	}

	// A spoken end at or past the next line's start never pushes the span past
	// that start: overlapping word timings cannot make a line swallow the next.
	late := []model.TimedWord{{Word: "раз", StartMS: 0, EndMS: 12000}}
	capped := ingest.ChunkTranslatedTranscriptByTime(translated, source, late)
	if capped[0].Span.EndMS != 10000 {
		t.Fatalf("late word end: got %d, want 10000", capped[0].Span.EndMS)
	}

	// Words that belong to a line with no translation counterpart (a marker the
	// translator did not keep) change nothing for the other lines.
	partial := "[00:00] one two\n[01:00] four"
	segs = ingest.ChunkTranslatedTranscriptByTime(partial, source, words)
	if len(segs) != 2 || segs[0].Span.EndMS != 1200 || segs[1].Span.StartMS != 60000 {
		t.Fatalf("partial markers: got %+v", segs)
	}
}
