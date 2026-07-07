package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// mustDropSet builds a subtitle.DropSet from configured patterns, failing the
// test on a compile error (the patterns here are always valid).
func mustDropSet(t *testing.T, patterns []string) *subtitle.DropSet {
	t.Helper()
	d, err := subtitle.NewDropSet(patterns)
	if err != nil {
		t.Fatalf("NewDropSet(%v): %v", patterns, err)
	}
	return d
}

// spamPhrases is the whisper keyword-spam vocabulary used across these tests.
var spamPhrases = []string{"Донбасс", "Крым", "Украина", "НАТО"}

// dropFixture is a time-stamped transcript whose middle line is pure keyword-spam
// (a whisper hallucination over non-speech); the surrounding lines are real
// speech that never mentions the spam words, so a drop pass leaves them clean.
const dropFixture = "[00:00] Welcome to the broadcast\n" +
	"[00:02] Донбасс, Крым, Украина, НАТО\n" +
	"[00:05] Сегодня обсуждаем экономику региона"

// scrubFixture pairs a real line with a mixed line where a leaked spam phrase is
// glued to real speech, plus a wholly-spam line that scrubs to nothing.
const scrubFixture = "[00:00] Welcome to the broadcast\n" +
	"[00:02] Сегодня обсуждаем Донбасс, Крым, Украина, НАТО экономику\n" +
	"[00:05] Донбасс, Крым, Украина, НАТО"

// TestDropScrubEmptyConfigUnchanged pins that empty/inactive drop and scrub sets
// leave the ingest chunk segments byte-for-byte identical to the unfiltered
// chunker, so the off-by-default path changes nothing at ingest.
func TestDropScrubEmptyConfigUnchanged(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(scrubFixture)
	empties := [][]string{nil, {}, {"  "}}
	for _, drop := range empties {
		for _, scrub := range empties {
			got := ingest.ApplyDropScrubToSegments(base, mustDropSet(t, drop), mustDropSet(t, scrub))
			if !reflect.DeepEqual(got, base) {
				t.Fatalf("empty drop/scrub changed chunks:\n got %#v\nwant %#v", got, base)
			}
		}
	}
	// Nil DropSet pointers are also a no-op.
	if got := ingest.ApplyDropScrubToSegments(base, nil, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("nil drop/scrub changed chunks:\n got %#v\nwant %#v", got, base)
	}
}

// TestDropSpamSegmentNeverEmbedded pins that a chunk composed entirely of
// configured drop phrases is removed at ingest, so pure keyword-spam never
// reaches the stored/embedded chunks, while real-speech chunks survive with
// their time spans intact.
func TestDropSpamSegmentNeverEmbedded(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(dropFixture)
	got := ingest.ApplyDropScrubToSegments(base, mustDropSet(t, spamPhrases), mustDropSet(t, nil))
	if len(got) == 0 {
		t.Fatal("expected surviving chunks, got none")
	}

	joined := joinSegText(got)
	// Every spam word from the dropped pure-spam line is gone from the index.
	for _, w := range spamPhrases {
		if strings.Contains(joined, w) {
			t.Fatalf("drop phrase %q leaked into ingest chunks: %q", w, joined)
		}
	}
	// Both real-speech lines survive.
	if !strings.Contains(joined, "Welcome to the broadcast") {
		t.Fatalf("expected welcome line to survive: %q", joined)
	}
	if !strings.Contains(joined, "экономику региона") {
		t.Fatalf("expected real content line to survive: %q", joined)
	}
	// Exactly the one pure-spam chunk was removed.
	if len(got) != len(base)-1 {
		t.Fatalf("expected exactly one chunk dropped: got %d, base %d", len(got), len(base))
	}
	for _, seg := range got {
		if seg.Span.Kind != "time" {
			t.Fatalf("surviving chunk lost its time span: %#v", seg.Span)
		}
	}
}

// TestScrubExcisesLeakedPhrase pins that a leaked spam phrase glued to real
// speech is excised at ingest (so the phrase is never embedded) while the
// sentence is kept, and that a wholly-spam chunk that scrubs to empty is dropped.
func TestScrubExcisesLeakedPhrase(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(scrubFixture)
	scrub := mustDropSet(t, []string{"Донбасс, Крым, Украина, НАТО"})

	got := ingest.ApplyDropScrubToSegments(base, mustDropSet(t, nil), scrub)
	joined := joinSegText(got)

	if strings.Contains(joined, "Донбасс, Крым, Украина, НАТО") {
		t.Fatalf("scrubbed phrase leaked into ingest chunks: %q", joined)
	}
	// The surrounding real speech of the mixed line is kept.
	if !strings.Contains(joined, "Сегодня обсуждаем") || !strings.Contains(joined, "экономику") {
		t.Fatalf("scrub removed real speech: %q", joined)
	}
	// The wholly-spam line (scrubFixture's third line) scrubs to empty and is
	// dropped: the real lines plus the scrubbed mixed line remain.
	if len(got) != len(base)-1 {
		t.Fatalf("expected the pure-spam line dropped after scrub: got %d, base %d", len(got), len(base))
	}
}

// TestScrubFiltersWordTimings pins that scrubbing a leaked phrase also excises
// the phrase's per-word timings from Span.Words — otherwise a downstream
// word-level consumer (broadcast re-segmentation) would rebuild the scrubbed
// spam from the leftover word timings, re-introducing what the scrub removed.
func TestScrubFiltersWordTimings(t *testing.T) {
	words := []model.TimedWord{
		{Word: "hello", StartMS: 0, EndMS: 400},
		{Word: "Крым", StartMS: 500, EndMS: 900}, // leaked spam token between real speech
		{Word: "world", StartMS: 1000, EndMS: 1400},
	}
	base := ingest.ChunkTranscriptByTimeWithWords("[00:00] hello Крым world", words)
	got := ingest.ApplyDropScrubToSegments(base, mustDropSet(t, nil), mustDropSet(t, []string{"Крым"}))

	var toks []string
	for _, seg := range got {
		for _, w := range seg.Span.Words {
			toks = append(toks, w.W)
			if strings.Contains(w.W, "Крым") {
				t.Fatalf("scrubbed word leaked into Span.Words: %v", toks)
			}
		}
	}
	// The real words keep their timings; only the scrubbed token is gone.
	if len(toks) != 2 {
		t.Fatalf("expected 2 surviving word timings (hello, world), got %v", toks)
	}
}

// TestDropScrubConsistentIngestAndExport pins that the same DropSet, applied to
// the same source, strips the same phrase at ingest (chunk segments) and at
// export (CleanCues) — the index and the exported sidecar agree cue-for-cue.
func TestDropScrubConsistentIngestAndExport(t *testing.T) {
	const text = "[00:00] keep this real line\n[00:02] Крым, НАТО"
	drop := mustDropSet(t, []string{"Крым", "НАТО"})

	// Ingest side: chunk, then apply the drop cleaning as the service does.
	ingestSegs := ingest.ApplyDropScrubToSegments(ingest.ChunkTranscriptByTime(text), drop, mustDropSet(t, nil))

	// Export side: build cues from the SAME chunks and clean on export.
	rawSegs := ingest.ChunkTranscriptByTime(text)
	chunks := make([]subtitle.TranscriptChunk, 0, len(rawSegs))
	for _, s := range rawSegs {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: s.Text, Span: s.Span})
	}
	exportCues := subtitle.CleanCues(subtitle.BuildCues(chunks), subtitle.CleanOptions{Drop: drop})

	ingestJoined := joinSegText(ingestSegs)
	var exportJoined strings.Builder
	for _, c := range exportCues {
		exportJoined.WriteString(c.Text)
		exportJoined.WriteString(" ")
	}

	if strings.Contains(ingestJoined, "НАТО") {
		t.Fatalf("drop phrase leaked at ingest: %q", ingestJoined)
	}
	if strings.Contains(exportJoined.String(), "НАТО") {
		t.Fatalf("drop phrase leaked at export: %q", exportJoined.String())
	}
	if !strings.Contains(ingestJoined, "keep this real line") {
		t.Fatalf("ingest dropped real content: %q", ingestJoined)
	}
	if !strings.Contains(exportJoined.String(), "keep this real line") {
		t.Fatalf("export dropped real content: %q", exportJoined.String())
	}
}

// TestDropScrubSidecarCues pins the sidecar-cue ingest path: a merged chunk with
// a leaked spam phrase is scrubbed while the real reporting is kept.
func TestDropScrubSidecarCues(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Real reporting here"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "Крым, НАТО"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	scrub := mustDropSet(t, []string{"Крым, НАТО"})

	got := ingest.ApplyDropScrubToSegments(segs, mustDropSet(t, nil), scrub)
	joined := joinSegText(got)
	if strings.Contains(joined, "Крым, НАТО") {
		t.Fatalf("scrubbed phrase leaked into sidecar chunks: %q", joined)
	}
	if !strings.Contains(joined, "Real reporting here") {
		t.Fatalf("expected real cue to survive: %q", joined)
	}
}
