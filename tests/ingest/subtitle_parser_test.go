package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

func TestChunkSubtitleCues_MergesCuesAndPreservesSpanBounds(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "alpha"},
		{Index: 2, StartMS: 1000, EndMS: 2500, Text: "beta"},
		{Index: 3, StartMS: 2500, EndMS: 4000, Text: "gamma"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	// Short cues merge into a single chunk; the span covers first start..last end.
	if len(segs) != 1 {
		t.Fatalf("expected 1 merged chunk, got %d", len(segs))
	}
	if segs[0].Span.Kind != "time" || segs[0].Span.StartMS != 0 || segs[0].Span.EndMS != 4000 {
		t.Fatalf("unexpected span: %+v", segs[0].Span)
	}
	if segs[0].Text != "alpha\nbeta\ngamma" {
		t.Fatalf("unexpected merged text: %q", segs[0].Text)
	}
}

func TestChunkSubtitleCues_SkipsEmptyCues(t *testing.T) {
	t.Parallel()
	cues := []subtitle.Cue{
		{StartMS: 0, EndMS: 1000, Text: "  "},
		{StartMS: 1000, EndMS: 2000, Text: "real"},
	}
	segs := ingest.ChunkSubtitleCues(cues)
	if len(segs) != 1 || segs[0].Text != "real" {
		t.Fatalf("expected single 'real' chunk, got %+v", segs)
	}
	if segs[0].Span.StartMS != 1000 || segs[0].Span.EndMS != 2000 {
		t.Fatalf("unexpected span: %+v", segs[0].Span)
	}
}

func assertCue(t *testing.T, got subtitle.Cue, wantIdx, wantStart, wantEnd int, wantText string) {
	t.Helper()
	if got.Index != wantIdx || got.StartMS != wantStart || got.EndMS != wantEnd || got.Text != wantText {
		t.Fatalf("cue mismatch: got {idx=%d start=%d end=%d text=%q} want {idx=%d start=%d end=%d text=%q}",
			got.Index, got.StartMS, got.EndMS, got.Text, wantIdx, wantStart, wantEnd, wantText)
	}
}

func TestParseVTT_BasicCues(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n\n00:00:00.000 --> 00:00:02.500\nHello world\n\n00:00:02.500 --> 00:00:05.000\nSecond line\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 0, 2500, "Hello world")
	assertCue(t, cues[1], 2, 2500, 5000, "Second line")
}

func TestParseVTT_SkipsNoteStyleAndIdentifiers(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n" +
		"\n" +
		"NOTE this is a comment\n" +
		"spanning two lines\n" +
		"\n" +
		"STYLE\n" +
		"::cue { color: yellow }\n" +
		"\n" +
		"cue-id-1\n" +
		"00:01.000 --> 00:02.000 align:start\n" +
		"<v Alice>Hi there</v>\n" +
		"\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d (%+v)", len(cues), cues)
	}
	// MM:SS form (no hours) and inline tags stripped; cue settings ignored.
	assertCue(t, cues[0], 1, 1000, 2000, "Hi there")
}

func TestParseVTT_MultiLineCueText(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\n\n00:00:00.000 --> 00:00:03.000\nline one\nline two\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 0, 3000, "line one\nline two")
}

func TestParseVTT_NoCuesReturnsError(t *testing.T) {
	t.Parallel()
	if _, err := subtitle.ParseVTT("WEBVTT\n\nNOTE only a note\n"); err == nil {
		t.Fatal("expected error for cue-less VTT")
	}
}

func TestParseSRT_BasicCuesWithCommaSeparator(t *testing.T) {
	t.Parallel()
	in := "1\n00:00:00,000 --> 00:00:02,000\nFirst\n\n2\n00:00:02,000 --> 00:00:04,250\nSecond\n"
	cues, err := subtitle.ParseSRT(in)
	if err != nil {
		t.Fatalf("ParseSRT: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 0, 2000, "First")
	assertCue(t, cues[1], 2, 2000, 4250, "Second")
}

func TestParseSRT_MissingIndexLineTolerated(t *testing.T) {
	t.Parallel()
	in := "00:00:01,000 --> 00:00:02,000\nNo index here\n"
	cues, err := subtitle.ParseSRT(in)
	if err != nil {
		t.Fatalf("ParseSRT: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 1000, 2000, "No index here")
}

func TestParseVTT_HoursAndCRLF(t *testing.T) {
	t.Parallel()
	in := "WEBVTT\r\n\r\n01:02:03.400 --> 01:02:04.000\r\nDeep into the file\r\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	want := ((1*60+2)*60+3)*1000 + 400
	assertCue(t, cues[0], 1, want, ((1*60+2)*60+4)*1000, "Deep into the file")
}

func TestParseSRT_LeadingBOMTolerated(t *testing.T) {
	t.Parallel()
	// A leading UTF-8 BOM (common in Windows-authored subtitles) must not block
	// the first cue's timing line from matching.
	in := "\uFEFF1\n00:00:00,000 --> 00:00:01,000\nWith BOM\n"
	cues, err := subtitle.ParseSRT(in)
	if err != nil {
		t.Fatalf("ParseSRT: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 0, 1000, "With BOM")
}

func TestParseVTT_LeadingBOMTolerated(t *testing.T) {
	t.Parallel()
	in := "\uFEFFWEBVTT\n\n00:00:00.000 --> 00:00:02.000\nHeader after BOM\n"
	cues, err := subtitle.ParseVTT(in)
	if err != nil {
		t.Fatalf("ParseVTT: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues))
	}
	assertCue(t, cues[0], 1, 0, 2000, "Header after BOM")
}

func TestParseTTML_Monolingual(t *testing.T) {
	t.Parallel()
	in := `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xml:lang="en">
  <body><div>
    <p begin="00:00:00.000" end="00:00:02.000">Hello</p>
    <p begin="2s" end="4s">World<br/>again</p>
  </div></body>
</tt>`
	cues, err := subtitle.ParseTTML(in)
	if err != nil {
		t.Fatalf("ParseTTML: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("expected 2 cues, got %d (%+v)", len(cues), cues)
	}
	assertCue(t, cues[0], 1, 0, 2000, "Hello")
	assertCue(t, cues[1], 2, 2000, 4000, "World\nagain")
}

func TestParseTTMLByLang_Bilingual(t *testing.T) {
	t.Parallel()
	in := `<tt xmlns="http://www.w3.org/ns/ttml">
  <body>
    <div xml:lang="en"><p begin="0s" end="1s">cat</p></div>
    <div xml:lang="fr"><p begin="0s" end="1s">chat</p></div>
  </body>
</tt>`
	groups, err := subtitle.ParseTTMLByLang(in)
	if err != nil {
		t.Fatalf("ParseTTMLByLang: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 language groups, got %d", len(groups))
	}
	// Sorted by language tag: "en" then "fr".
	if groups[0].Lang != "en" || len(groups[0].Cues) != 1 || groups[0].Cues[0].Text != "cat" {
		t.Fatalf("unexpected en group: %+v", groups[0])
	}
	if groups[1].Lang != "fr" || len(groups[1].Cues) != 1 || groups[1].Cues[0].Text != "chat" {
		t.Fatalf("unexpected fr group: %+v", groups[1])
	}
}

func TestParseTTML_OffsetTimes(t *testing.T) {
	t.Parallel()
	in := `<tt xmlns="http://www.w3.org/ns/ttml">
  <body><div>
    <p begin="500ms" end="1500ms">half second start</p>
  </div></body>
</tt>`
	cues, err := subtitle.ParseTTML(in)
	if err != nil {
		t.Fatalf("ParseTTML: %v", err)
	}
	assertCue(t, cues[0], 1, 500, 1500, "half second start")
}
