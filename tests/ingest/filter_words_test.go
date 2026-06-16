package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
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

func joinSegText(segs []ingest.ChunkSegment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, " | ")
}
