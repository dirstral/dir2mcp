package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC 8.6.1 names one authored cue as the transcript segment of a subtitle
// sidecar, and a chunk window MUST close when the next segment would make it
// longer than media.transcript_chunk_sec. Before dir2mcp #1096 the sidecar path
// packed cues to 1200 characters first, and the window, which only merges,
// could not split a packed chunk. In sparse speech one chunk then spanned many
// minutes. These tests pin the bound through the real sidecar ingest path.

// testCue is one authored cue of a generated WebVTT sidecar.
type testCue struct {
	startMS int
	endMS   int
	text    string
}

// vttTimestamp renders ms as a WebVTT HH:MM:SS.mmm timestamp.
func vttTimestamp(ms int) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, (ms/60000)%60, (ms/1000)%60, ms%1000)
}

// renderVTT renders cues as a WebVTT document.
func renderVTT(cues []testCue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n", vttTimestamp(c.startMS), vttTimestamp(c.endMS), c.text)
	}
	return b.String()
}

// sparseCues returns 40 short cues of 2 s each, one every 45 s, so they spread
// over 30 minutes with long silences between them.
func sparseCues() []testCue {
	cues := make([]testCue, 0, 40)
	for i := 0; i < 40; i++ {
		start := i * 45000
		cues = append(cues, testCue{startMS: start, endMS: start + 2000, text: fmt.Sprintf("Line %d.", i)})
	}
	return cues
}

// ingestSidecarChunks writes cues as the sidecar of one audio file, ingests it
// through IngestSidecarTranscripts under cfg, and returns the persisted chunks
// and their spans (one span per chunk). STT must not run.
func ingestSidecarChunks(t *testing.T, cfg config.Config, cues []testCue) ([]model.Chunk, []model.Span) {
	t.Helper()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "media", "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "talk.vtt"), renderVTT(cues))

	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&explodingTranscriber{t: t})

	doc := model.Document{DocID: 1, RelPath: "media/talk.mp3", DocType: "audio"}
	ingested, err := svc.IngestSidecarTranscripts(context.Background(), doc)
	if err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if !ingested {
		t.Fatal("expected the sidecar to be ingested")
	}
	if len(st.chunks) == 0 || len(st.chunks) != len(st.spans) {
		t.Fatalf("got %d chunks and %d spans, want one span per chunk and at least one chunk",
			len(st.chunks), len(st.spans))
	}
	return st.chunks, st.spans
}

// longestCueMS returns the duration of the longest cue in cues.
func longestCueMS(cues []testCue) int {
	longest := 0
	for _, c := range cues {
		if d := c.endMS - c.startMS; d > longest {
			longest = d
		}
	}
	return longest
}

// TestSidecarChunkWindow_SparseCuesStayWithinWindow is the #1096 report: with
// the default window, 40 cues over 30 minutes must not collapse into one chunk
// that spans the whole recording.
func TestSidecarChunkWindow_SparseCuesStayWithinWindow(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	windowMS := cfg.MediaTranscriptChunkSec * 1000
	cues := sparseCues()

	chunks, spans := ingestSidecarChunks(t, cfg, cues)
	for i, sp := range spans {
		if sp.Kind != "time" {
			t.Fatalf("chunk %d: span kind %q, want time", i, sp.Kind)
		}
		if d := sp.EndMS - sp.StartMS; d > windowMS {
			t.Errorf("chunk %d spans %d ms [%d,%d], over the %d ms window", i, d, sp.StartMS, sp.EndMS, windowMS)
		}
	}
	// Every cue is 43 s of silence from the next, far over the 6 s gap, so each
	// cue is its own chunk and keeps its own authored span.
	if len(chunks) != len(cues) {
		t.Fatalf("got %d chunks, want %d (one per isolated cue)", len(chunks), len(cues))
	}
	for i, c := range cues {
		if spans[i].StartMS != c.startMS || spans[i].EndMS != c.endMS {
			t.Errorf("chunk %d span [%d,%d], want the cue's [%d,%d]",
				i, spans[i].StartMS, spans[i].EndMS, c.startMS, c.endMS)
		}
		if chunks[i].Text != c.text {
			t.Errorf("chunk %d text %q, want %q", i, chunks[i].Text, c.text)
		}
	}
}

// TestSidecarChunkWindow_SparseCuesWithoutGapRule proves the duration rule on
// its own: with the silence rule off, the sparse cues still never merge into a
// chunk longer than the window.
func TestSidecarChunkWindow_SparseCuesWithoutGapRule(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.MediaTranscriptChunkSec = 60
	cfg.MediaTranscriptChunkGapSec = 0
	cues := sparseCues()

	chunks, spans := ingestSidecarChunks(t, cfg, cues)
	merged := 0
	for i, sp := range spans {
		if d := sp.EndMS - sp.StartMS; d > 60000 {
			t.Errorf("chunk %d spans %d ms, over the 60 s window", i, d)
		}
		if len(sp.Cues) > 1 {
			merged++
		}
	}
	// Two cues 45 s apart fit a 60 s window (47 s span); three do not (92 s).
	if merged == 0 || len(chunks) >= len(cues) {
		t.Fatalf("got %d chunks (%d merged) from %d cues; the duration rule alone must still merge pairs",
			len(chunks), merged, len(cues))
	}
}

// TestSidecarChunkWindow_SingleLongCueIsTheException pins the one allowed
// overrun: an authored cue longer than the window stays one whole chunk.
func TestSidecarChunkWindow_SingleLongCueIsTheException(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	windowMS := cfg.MediaTranscriptChunkSec * 1000
	cues := append(sparseCues()[:3], testCue{startMS: 200000, endMS: 290000, text: "A ninety second cue."})
	if longestCueMS(cues) <= windowMS {
		t.Fatal("the long cue must be longer than the window, or this proves nothing")
	}

	_, spans := ingestSidecarChunks(t, cfg, cues)
	sawLong := false
	for i, sp := range spans {
		d := sp.EndMS - sp.StartMS
		if d <= windowMS {
			continue
		}
		if sp.StartMS != 200000 || sp.EndMS != 290000 || len(sp.Cues) > 1 {
			t.Errorf("chunk %d spans %d ms [%d,%d] with %d cues; only the single long cue may exceed the window",
				i, d, sp.StartMS, sp.EndMS, len(sp.Cues))
		}
		sawLong = true
	}
	if !sawLong {
		t.Fatal("the long cue did not reach the index as its own chunk")
	}
}

// TestSidecarChunkWindow_DenseCuesPackUpToWindow pins that the fix does not
// regress dense speech to one chunk per cue: contiguous 4 s cues still merge up
// to the window, and each merged chunk records its authored cues for export.
func TestSidecarChunkWindow_DenseCuesPackUpToWindow(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	windowMS := cfg.MediaTranscriptChunkSec * 1000
	cues := make([]testCue, 0, 30)
	for i := 0; i < 30; i++ {
		cues = append(cues, testCue{startMS: i * 4000, endMS: (i + 1) * 4000, text: fmt.Sprintf("Word %d.", i)})
	}

	chunks, spans := ingestSidecarChunks(t, cfg, cues)
	// 120 s of contiguous 4 s cues fill three 40 s windows exactly.
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks from %d dense cues, want 3 full 40 s windows", len(chunks), len(cues))
	}
	for i, sp := range spans {
		if d := sp.EndMS - sp.StartMS; d != windowMS {
			t.Errorf("chunk %d spans %d ms, want the full %d ms window", i, d, windowMS)
		}
		if len(sp.Cues) != 10 {
			t.Fatalf("chunk %d records %d cues, want its 10 authored cues", i, len(sp.Cues))
		}
		for j, rec := range sp.Cues {
			authored := cues[i*10+j]
			if rec.T != authored.startMS || rec.D != authored.endMS-authored.startMS ||
				rec.N != utf8.RuneCountInString(authored.text) {
				t.Errorf("chunk %d cue %d record %+v does not match the authored cue %+v", i, j, rec, authored)
			}
		}
	}
	// The recorded cues cut the merged text back into the authored cues.
	exported := subtitle.BuildCues([]subtitle.TranscriptChunk{{Text: chunks[0].Text, Span: spans[0]}})
	if len(exported) != 10 || exported[0].Text != cues[0].text {
		t.Fatalf("export of a merged sidecar chunk gave %d cues (first %q), want the 10 authored cues",
			len(exported), firstCueText(exported))
	}
}

// firstCueText returns the text of the first cue, or "" when there is none.
func firstCueText(cues []subtitle.Cue) string {
	if len(cues) == 0 {
		return ""
	}
	return cues[0].Text
}

// TestSidecarChunkWindow_CharacterCapStillHolds pins the 1200-rune cap as an
// upper bound: long contiguous cues that fit the window by time still close a
// chunk before its text grows past TranscriptChunkMaxChars.
func TestSidecarChunkWindow_CharacterCapStillHolds(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	long := strings.Repeat("a", 299) + "."
	cues := make([]testCue, 0, 30)
	for i := 0; i < 30; i++ {
		cues = append(cues, testCue{startMS: i * 1000, endMS: (i + 1) * 1000, text: long})
	}

	chunks, spans := ingestSidecarChunks(t, cfg, cues)
	merged := 0
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c.Text); n > ingest.TranscriptChunkMaxChars {
			t.Errorf("chunk %d is %d runes, over the %d cap", i, n, ingest.TranscriptChunkMaxChars)
		}
		if len(spans[i].Cues) > 1 {
			merged++
		}
	}
	// 30 s of cues fit one 40 s window by time, so only the cap splits them:
	// three 300-rune cues plus two spaces is 902 runes, a fourth would be 1203.
	if len(chunks) != 10 || merged != 10 {
		t.Fatalf("got %d chunks (%d merged), want 10 chunks of 3 cues each under the character cap",
			len(chunks), merged)
	}
}

// TestSidecarChunkWindow_DisabledKeepsCharacterPacking pins the documented
// escape hatch: media.transcript_chunk_sec 0 keeps the pre-window sidecar
// chunks, packed to the character cap with no duration bound and no cue record.
func TestSidecarChunkWindow_DisabledKeepsCharacterPacking(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.MediaTranscriptChunkSec = 0
	cues := sparseCues()

	chunks, spans := ingestSidecarChunks(t, cfg, cues)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want the 1 character-packed chunk of the pre-window behavior", len(chunks))
	}
	last := cues[len(cues)-1]
	if spans[0].StartMS != 0 || spans[0].EndMS != last.endMS {
		t.Errorf("packed span [%d,%d], want [0,%d]", spans[0].StartMS, spans[0].EndMS, last.endMS)
	}
	if len(spans[0].Cues) != 0 {
		t.Errorf("an unmerged chunk must carry no cue record, got %d", len(spans[0].Cues))
	}
	if !strings.HasPrefix(chunks[0].Text, "Line 0.\nLine 1.\n") {
		t.Errorf("packed cues must join with a newline as before, got %q", chunks[0].Text)
	}
}
