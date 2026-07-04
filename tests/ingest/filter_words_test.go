package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// transcriptFixture is a small time-stamped transcript whose middle line is
// pure boilerplate (a credit/watermark line) plus a trailing-phrase line.
const transcriptFixture = "[00:00] Welcome to the show\n" +
	"[00:02] Subscribe to our channel\n" +
	"[00:05] Today we discuss Subscribe to our channel deterministic exports"

// TestTranscriptChunkingEmptyFilterUnchanged pins that an empty/inactive filter
// produces output identical to the unfiltered chunker, so the empty-config path
// changes nothing.
func TestTranscriptChunkingEmptyFilterUnchanged(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(transcriptFixture)
	for _, f := range []*subtitle.WordFilter{nil, subtitle.NewWordFilter(nil), subtitle.NewWordFilter([]string{"  "})} {
		got := ingest.ChunkTranscriptByTimeFiltered(transcriptFixture, f)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("empty filter changed transcript chunks:\n got %#v\nwant %#v", got, base)
		}
	}
}

// TestTranscriptChunkingStripsPhrases pins that configured phrases are removed
// from transcript chunk text (so they never get embedded), that a chunk which
// is pure boilerplate is dropped, and that time spans on survivors are intact.
func TestTranscriptChunkingStripsPhrases(t *testing.T) {
	f := subtitle.NewWordFilter([]string{"Subscribe to our channel"})
	got := ingest.ChunkTranscriptByTimeFiltered(transcriptFixture, f)

	if len(got) == 0 {
		t.Fatal("expected surviving chunks, got none")
	}
	for _, seg := range got {
		if strings.Contains(strings.ToLower(seg.Text), "subscribe to our channel") {
			t.Fatalf("filtered phrase leaked into chunk text: %q", seg.Text)
		}
		if seg.Span.Kind != "time" {
			t.Fatalf("chunk lost its time span: %#v", seg.Span)
		}
	}
	// The boilerplate-only line ("Subscribe to our channel" at 00:02) must be
	// dropped, so its text must not appear anywhere and the welcome + final lines
	// survive.
	joined := strings.ToLower(joinSegText(got))
	if !strings.Contains(joined, "welcome to the show") {
		t.Fatalf("expected welcome line to survive: %q", joined)
	}
	if !strings.Contains(joined, "today we discuss") || !strings.Contains(joined, "deterministic exports") {
		t.Fatalf("expected final line to survive with phrase removed: %q", joined)
	}
}

// TestSubtitleCueChunkingFilter pins the sidecar-cue chunker: a cue that is pure
// boilerplate is dropped (its time span never merges into a chunk) and inline
// phrases are stripped, case-insensitively.
func TestSubtitleCueChunkingFilter(t *testing.T) {
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "Hello there"},
		{Index: 2, StartMS: 1000, EndMS: 2000, Text: "SUBSCRIBE NOW"},
		{Index: 3, StartMS: 2000, EndMS: 3000, Text: "Real content subscribe now here"},
	}
	f := subtitle.NewWordFilter([]string{"subscribe now"})
	got := ingest.ChunkSubtitleCuesFiltered(cues, f)

	joined := strings.ToLower(joinSegText(got))
	if strings.Contains(joined, "subscribe now") {
		t.Fatalf("filtered phrase leaked into sidecar chunks: %q", joined)
	}
	if !strings.Contains(joined, "hello there") || !strings.Contains(joined, "real content") {
		t.Fatalf("expected real cues to survive: %q", joined)
	}

	// Empty/inactive filter is identical to the unfiltered chunker.
	base := ingest.ChunkSubtitleCues(cues)
	if got2 := ingest.ChunkSubtitleCuesFiltered(cues, subtitle.NewWordFilter(nil)); !reflect.DeepEqual(got2, base) {
		t.Fatalf("inactive filter changed sidecar chunks:\n got %#v\nwant %#v", got2, base)
	}
}

// TestFilterConsistentIngestAndExport pins that the same filter applied to the
// same source text strips the same phrase at ingest (transcript chunking) and at
// export (cue filtering). Building cues from the filtered transcript chunks and
// re-filtering them on export must not contain the phrase either way.
func TestFilterConsistentIngestAndExport(t *testing.T) {
	const text = "[00:00] keep this line\n[00:02] drop watermark only"
	f := subtitle.NewWordFilter([]string{"watermark only"})

	// Ingest side: chunk with filter.
	ingestSegs := ingest.ChunkTranscriptByTimeFiltered(text, f)

	// Export side: build cues from the SAME unfiltered chunks and filter on
	// export. Both paths must agree that "watermark only" is gone.
	rawSegs := ingest.ChunkTranscriptByTime(text)
	chunks := make([]subtitle.TranscriptChunk, 0, len(rawSegs))
	for _, s := range rawSegs {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: s.Text, Span: s.Span})
	}
	exportCues := subtitle.FilterCues(subtitle.BuildCues(chunks), f)

	ingestJoined := strings.ToLower(joinSegText(ingestSegs))
	var exportJoined strings.Builder
	for _, c := range exportCues {
		exportJoined.WriteString(strings.ToLower(c.Text))
		exportJoined.WriteString(" ")
	}
	if strings.Contains(ingestJoined, "watermark only") {
		t.Fatalf("phrase leaked at ingest: %q", ingestJoined)
	}
	if strings.Contains(exportJoined.String(), "watermark only") {
		t.Fatalf("phrase leaked at export: %q", exportJoined.String())
	}
	// Both keep the real line.
	if !strings.Contains(ingestJoined, "keep this line") {
		t.Fatalf("ingest dropped real content: %q", ingestJoined)
	}
	if !strings.Contains(exportJoined.String(), "keep this line") {
		t.Fatalf("export dropped real content: %q", exportJoined.String())
	}
}

// TestFilterWordsDroppedFromSpanWords pins FIX 2: filtered phrases are removed
// from the attached per-word timings (Span.Words), not just from segment Text.
// Broadcast cues are rebuilt from Span.Words, so a leak here re-introduces the
// watermark/credit phrase the filter stripped — and a re-segmentation that splits
// the phrase across a cue boundary defeats the export-time substring backstop.
// Filtering at the source (the word list) closes the leak for both.
func TestFilterWordsDroppedFromSpanWords(t *testing.T) {
	// One timestamped segment; the words carry a filter phrase with a >600ms gap
	// INSIDE it ("Subscribe" ... pause ... "to our channel"), so broadcast
	// re-segmentation would split the phrase across two cues — exactly the case a
	// substring FilterCues backstop cannot catch.
	const content = "[00:00] Hello world Subscribe to our channel today"
	words := []model.TimedWord{
		{Word: "Hello", StartMS: 0, EndMS: 400},
		{Word: "world", StartMS: 400, EndMS: 1400},
		{Word: "Subscribe", StartMS: 1400, EndMS: 1800},
		{Word: "to", StartMS: 2600, EndMS: 2800}, // 800ms gap -> segmentation break mid-phrase
		{Word: "our", StartMS: 2800, EndMS: 3000},
		{Word: "channel", StartMS: 3000, EndMS: 3200},
		{Word: "today", StartMS: 4000, EndMS: 4400},
	}
	f := subtitle.NewWordFilter([]string{"Subscribe to our channel"})

	segs := ingest.ChunkTranscriptByTimeWithWordsFiltered(content, words, f)

	// Source of truth: the filtered tokens must be gone from Span.Words.
	chunks := make([]subtitle.TranscriptChunk, 0, len(segs))
	for _, s := range segs {
		for _, w := range s.Span.Words {
			lw := strings.ToLower(w.W)
			if lw == "subscribe" || lw == "channel" {
				t.Fatalf("filtered word %q leaked into Span.Words", w.W)
			}
		}
		chunks = append(chunks, subtitle.TranscriptChunk{Text: s.Text, Span: s.Span})
	}

	// Consumer: broadcast cues rebuilt from Span.Words must not contain the phrase
	// (nor its straddling fragments), WITHOUT relying on the export FilterCues
	// backstop.
	cues := subtitle.BuildBroadcastCues(chunks)
	if len(cues) == 0 {
		t.Fatal("expected broadcast cues from the surviving words, got none")
	}
	var joined strings.Builder
	for _, c := range cues {
		joined.WriteString(strings.ToLower(c.Text))
		joined.WriteString(" ")
	}
	got := joined.String()
	for _, banned := range []string{"subscribe", "channel"} {
		if strings.Contains(got, banned) {
			t.Fatalf("filtered phrase fragment %q leaked into broadcast cues: %q", banned, got)
		}
	}
	// The real content survives.
	if !strings.Contains(got, "hello world") {
		t.Fatalf("real content dropped from broadcast cues: %q", got)
	}
}

func joinSegText(segs []ingest.ChunkSegment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, " | ")
}
