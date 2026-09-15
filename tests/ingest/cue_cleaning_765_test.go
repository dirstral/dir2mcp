package tests

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// Issue #765: media.subtitles.drop_urls and collapse_repeats used to run only on
// the export path, so a hallucinated URL cue or a repetition artifact was stripped
// from the exported sidecar and left embedded: retrievable, and citable for a span
// where nobody said anything. These tests pin that both now clean the chunk text
// at ingest, in the same order the export pass uses.

// urlFixture is a time-stamped transcript whose middle line is the credit-line /
// bare-domain hallucination whisper emits over silence or music.
const urlFixture = "[00:00] Welcome to the broadcast\n" +
	"[00:02] Subtitles by www.example.com\n" +
	"[00:05] Today we discuss the regional economy"

// TestCueCleaningEmptyConfigUnchanged pins that inactive options leave the ingest
// chunk segments byte-for-byte identical, so the off-by-default path changes
// nothing. A glossary-only configuration counts as inactive at ingest: the
// glossary is deliberately not part of the ingest pass (SPEC §8.6.2 pins it as
// export-time), so configuring one must not perturb the index.
func TestCueCleaningEmptyConfigUnchanged(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(urlFixture)
	glossary, err := subtitle.NewGlossary([]string{"economy=>economics"})
	if err != nil {
		t.Fatalf("NewGlossary: %v", err)
	}
	for name, opts := range map[string]subtitle.CleanOptions{
		"zero":          {},
		"collapse-of-1": {CollapseRepeats: 1},
		"glossary-only": {Glossary: glossary},
	} {
		got := ingest.ApplyCueCleaningToSegments(base, opts)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("%s options changed chunks:\n got %#v\nwant %#v", name, got, base)
		}
	}
}

// TestDropURLsNeverEmbedded pins that a hallucinated URL / credit-line chunk is
// removed at ingest, so it is never embedded and can never be cited, while the
// surrounding real speech keeps its text and time span.
func TestDropURLsNeverEmbedded(t *testing.T) {
	base := ingest.ChunkTranscriptByTime(urlFixture)
	got := ingest.ApplyCueCleaningToSegments(base, subtitle.CleanOptions{DropURLs: true})

	joined := joinSegText(got)
	if strings.Contains(joined, "example.com") {
		t.Fatalf("hallucinated URL leaked into ingest chunks: %q", joined)
	}
	if len(got) != len(base)-1 {
		t.Fatalf("expected exactly the URL chunk dropped: got %d, base %d", len(got), len(base))
	}
	if !strings.Contains(joined, "Welcome to the broadcast") ||
		!strings.Contains(joined, "regional economy") {
		t.Fatalf("drop_urls removed real speech: %q", joined)
	}
	for _, seg := range got {
		if seg.Span.Kind != "time" {
			t.Fatalf("surviving chunk lost its time span: %#v", seg.Span)
		}
	}
}

// TestCollapseRepeatsAtIngest pins that a run of identical consecutive chunks is
// collapsed before embedding: the first collapse_repeats-1 survive and the rest
// are dropped, so a repetition artifact cannot dominate a document's chunks.
func TestCollapseRepeatsAtIngest(t *testing.T) {
	const repeated = "[00:00] Real opening line\n" +
		"[00:02] Продолжение следует\n" +
		"[00:04] Продолжение следует\n" +
		"[00:06] Продолжение следует\n" +
		"[00:08] Продолжение следует\n" +
		"[00:10] Real closing line"
	base := ingest.ChunkTranscriptByTime(repeated)
	got := ingest.ApplyCueCleaningToSegments(base, subtitle.CleanOptions{CollapseRepeats: 3})

	var repeats int
	for _, seg := range got {
		if strings.Contains(seg.Text, "Продолжение следует") {
			repeats++
		}
	}
	if repeats != 2 {
		t.Fatalf("expected the first 2 of 4 identical chunks to survive, got %d: %q", repeats, joinSegText(got))
	}
	joined := joinSegText(got)
	if !strings.Contains(joined, "Real opening line") || !strings.Contains(joined, "Real closing line") {
		t.Fatalf("collapse dropped non-repeated speech: %q", joined)
	}
	// The run is broken by the closing line, so a later identical text would start
	// a fresh run rather than continuing the collapsed one.
	if len(got) != len(base)-2 {
		t.Fatalf("expected exactly 2 chunks dropped: got %d, base %d", len(got), len(base))
	}
}

// TestDropURLsRunsBeforeScrub pins the ordering of the two whole-chunk passes:
// drop_urls sees the ORIGINAL text. Here the scrub phrase would excise the very
// token that identifies the chunk as a credit line, so scrubbing first would keep
// a chunk that the export pass drops, and the index and the sidecar would
// disagree. Both surfaces must reach the same verdict.
func TestDropURLsRunsBeforeScrub(t *testing.T) {
	const text = "[00:00] Real reporting here\n[00:02] Смотрите нас на example.com"
	opts := subtitle.CleanOptions{
		DropURLs: true,
		Scrub:    mustDropSet(t, []string{`example\.com`}),
	}

	got := ingest.ApplyCueCleaningToSegments(ingest.ChunkTranscriptByTime(text), opts)
	joined := joinSegText(got)
	if strings.Contains(joined, "Смотрите нас") {
		t.Fatalf("credit-line chunk survived the scrub instead of being dropped: %q", joined)
	}
	if !strings.Contains(joined, "Real reporting here") {
		t.Fatalf("expected the real chunk to survive: %q", joined)
	}

	// Export makes the same call on the same chunks.
	exportCues := subtitle.CleanCues(subtitle.BuildCues(chunksFromSegments(ingest.ChunkTranscriptByTime(text))), opts)
	if len(exportCues) != len(got) {
		t.Fatalf("ingest kept %d chunks, export kept %d cues: the two disagree", len(got), len(exportCues))
	}
}

// TestCollapseRepeatsRunsAfterScrub pins the other half of the ordering: the run
// is counted on the POST-scrub text that is actually stored. Two chunks that
// differ only by a leaked hallucination phrase are the same cue once scrubbed, so
// counting the run before the scrub would leave the duplicate in the index while
// export (which scrubs first) drops it.
func TestCollapseRepeatsRunsAfterScrub(t *testing.T) {
	const text = "[00:00] Мы продолжаем\n" +
		"[00:02] Мы продолжаем Крым, НАТО\n" +
		"[00:04] Real closing line"
	opts := subtitle.CleanOptions{
		Scrub:           mustDropSet(t, []string{"Крым, НАТО"}),
		CollapseRepeats: 2,
	}

	got := ingest.ApplyCueCleaningToSegments(ingest.ChunkTranscriptByTime(text), opts)
	var repeats int
	for _, seg := range got {
		if strings.TrimSpace(seg.Text) == "Мы продолжаем" {
			repeats++
		}
	}
	if repeats != 1 {
		t.Fatalf("expected the scrubbed duplicate to be collapsed, got %d copies: %q", repeats, joinSegText(got))
	}
	if !strings.Contains(joinSegText(got), "Real closing line") {
		t.Fatalf("collapse dropped real speech: %q", joinSegText(got))
	}
}

// TestCueCleaningIngestAndExportAgree pins the invariant #545 stated and #765
// completes: with the same options, the text the index keeps and the text the
// exported sidecar keeps are the same, cue for cue.
func TestCueCleaningIngestAndExportAgree(t *testing.T) {
	const text = "[00:00] Реальный репортаж\n" +
		"[00:02] www.example.com\n" +
		"[00:04] Повтор\n" +
		"[00:06] Повтор\n" +
		"[00:08] Повтор\n" +
		"[00:10] Elola alolo."
	script, err := subtitle.NewScriptGuard("cyrillic")
	if err != nil {
		t.Fatalf("NewScriptGuard: %v", err)
	}
	opts := subtitle.CleanOptions{DropURLs: true, CollapseRepeats: 2, Script: script}

	ingestSegs := ingest.ApplyCueCleaningToSegments(ingest.ChunkTranscriptByTime(text), opts)
	exportCues := subtitle.CleanCues(subtitle.BuildCues(chunksFromSegments(ingest.ChunkTranscriptByTime(text))), opts)

	ingestTexts := make([]string, 0, len(ingestSegs))
	for _, seg := range ingestSegs {
		ingestTexts = append(ingestTexts, strings.TrimSpace(seg.Text))
	}
	exportTexts := make([]string, 0, len(exportCues))
	for _, c := range exportCues {
		exportTexts = append(exportTexts, strings.TrimSpace(c.Text))
	}
	if !reflect.DeepEqual(ingestTexts, exportTexts) {
		t.Fatalf("index and sidecar disagree:\n index  %q\n export %q", ingestTexts, exportTexts)
	}
	if len(ingestTexts) != 2 {
		t.Fatalf("expected the URL and wrong-script chunks dropped and the repeat run collapsed to one, got %q", ingestTexts)
	}
}

// chunksFromSegments adapts ingest chunk segments to the export cue builder's
// input shape, so a test can clean the very same chunks on both surfaces.
func chunksFromSegments(segs []ingest.ChunkSegment) []subtitle.TranscriptChunk {
	out := make([]subtitle.TranscriptChunk, 0, len(segs))
	for _, s := range segs {
		out = append(out, subtitle.TranscriptChunk{Text: s.Text, Span: s.Span})
	}
	return out
}

// longCue pads text to more than TranscriptChunkMaxChars so one cue becomes one
// stored chunk. The cue unit at ingest IS the stored chunk (export re-derives its
// cues from these chunks), so a fixture that merges every cue into a single chunk
// could not tell a per-cue verdict apart from a whole-document one.
func longCue(text string) string {
	return text + " " + strings.Repeat("подробности передачи ", 70)
}

// TestSidecarIngest_DropsHallucinatedURLCue drives the full sidecar ingest path
// with media.subtitles.drop_urls enabled and pins that the credit-line cue never
// reaches a stored chunk, while the real cues do.
func TestSidecarIngest_DropsHallucinatedURLCue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "media", "lecture.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "lecture.vtt"),
		"WEBVTT\n\n"+
			"00:00:00.000 --> 00:00:20.000\n"+longCue("Реальный репортаж")+"\n\n"+
			"00:00:20.000 --> 00:00:22.000\nSubtitles by www.example.com\n\n"+
			"00:00:22.000 --> 00:00:40.000\n"+longCue("Продолжение репортажа")+"\n")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{
		RootDir:                root,
		StateDir:               t.TempDir(),
		MediaSubtitlesDropURLs: true,
	}, st)

	doc := model.Document{DocID: 1, RelPath: "media/lecture.mp3", DocType: "audio"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.chunks) != 2 {
		t.Fatalf("expected 2 stored chunks (the URL cue dropped), got %d", len(st.chunks))
	}
	for _, c := range st.chunks {
		if strings.Contains(c.Text, "example.com") {
			t.Fatalf("hallucinated URL cue was embedded: %q", c.Text)
		}
	}
}

// TestSidecarIngest_DropsWrongScriptCue drives the full sidecar ingest path with
// media.subtitles.expect_script set and pins that a wrong-script gibberish cue
// never reaches a stored chunk, while the real cues and a digit-bearing cue do
// (SPEC §8.6.3: the index and the export apply the same rule).
func TestSidecarIngest_DropsWrongScriptCue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "media", "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "talk.vtt"),
		"WEBVTT\n\n"+
			"00:00:00.000 --> 00:00:20.000\n"+longCue("Реальный репортаж")+"\n\n"+
			"00:00:20.000 --> 00:00:22.000\nElola alolo.\n\n"+
			"00:00:22.000 --> 00:00:40.000\n"+longCue("Продолжение репортажа")+"\n\n"+
			// A long cue on each side keeps the two short cues in separate
			// chunks: two adjacent short cues merge into one chunk, and the
			// digit in one would then protect the gibberish in the other.
			"00:00:40.000 --> 00:00:42.000\nCOVID-19.\n\n"+
			"00:00:42.000 --> 00:01:00.000\n"+longCue("Итоги репортажа")+"\n")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{
		RootDir:                    root,
		StateDir:                   t.TempDir(),
		MediaSubtitlesExpectScript: "cyrillic",
	}, st)

	doc := model.Document{DocID: 1, RelPath: "media/talk.mp3", DocType: "audio"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.chunks) != 4 {
		t.Fatalf("expected 4 stored chunks (the wrong-script cue dropped, the digit cue kept), got %d", len(st.chunks))
	}
	kept := false
	for _, c := range st.chunks {
		if strings.Contains(c.Text, "Elola") {
			t.Fatalf("wrong-script cue was embedded: %q", c.Text)
		}
		if strings.Contains(c.Text, "COVID-19") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the digit-bearing cue must survive the script guard")
	}
}

// TestSidecarIngest_CollapsesRepeatedCues drives the full sidecar ingest path
// with media.subtitles.collapse_repeats enabled and pins that only the first
// collapse_repeats-1 chunks of an identical run are stored.
func TestSidecarIngest_CollapsesRepeatedCues(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repeated := longCue("Продолжение следует")
	writeFile(t, filepath.Join(root, "media", "clip.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "media", "clip.vtt"),
		"WEBVTT\n\n"+
			"00:00:00.000 --> 00:00:20.000\n"+repeated+"\n\n"+
			"00:00:20.000 --> 00:00:40.000\n"+repeated+"\n\n"+
			"00:00:40.000 --> 00:01:00.000\n"+repeated+"\n\n"+
			"00:01:00.000 --> 00:01:20.000\n"+repeated+"\n")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{
		RootDir:                       root,
		StateDir:                      t.TempDir(),
		MediaSubtitlesCollapseRepeats: 3,
	}, st)

	doc := model.Document{DocID: 1, RelPath: "media/clip.mp3", DocType: "audio"}
	if _, err := svc.IngestSidecarTranscripts(context.Background(), doc); err != nil {
		t.Fatalf("IngestSidecarTranscripts: %v", err)
	}
	if len(st.chunks) != 2 {
		t.Fatalf("expected the 4-cue run collapsed to 2 stored chunks, got %d", len(st.chunks))
	}
}

// TestNewService_RefusesInvalidCueCleaning pins that a Service cannot be built
// with a cleaning rule subtitle refuses (#994 review). The old shape logged the
// bad rule and ran with that filter off, so a caller that skipped
// config.Validate got an index with the gibberish it had configured away.
func TestNewService_RefusesInvalidCueCleaning(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]config.Config{
		"unknown expect_script":    {StateDir: t.TempDir(), MediaSubtitlesExpectScript: "klingon"},
		"bad drop_phrases regexp":  {StateDir: t.TempDir(), MediaSubtitlesDropPhrases: []string{"a(b"}},
		"bad scrub_phrases regexp": {StateDir: t.TempDir(), MediaSubtitlesScrubPhrases: []string{"a(b"}},
	} {
		_, err := ingest.NewService(cfg, &fakeIngestStore{})
		if err == nil {
			t.Errorf("%s: NewService must fail instead of running with the filter off", name)
			continue
		}
		if !strings.Contains(err.Error(), "media.subtitles.") {
			t.Errorf("%s: the error must name the key: %v", name, err)
		}
	}
	if _, err := ingest.NewService(config.Config{StateDir: t.TempDir(), MediaSubtitlesExpectScript: "cyrillic"}, &fakeIngestStore{}); err != nil {
		t.Fatalf("a valid rule must still build: %v", err)
	}
}
