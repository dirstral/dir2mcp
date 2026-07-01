package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// sampleCues is a fixed set of cues exercising sub-hour and over-hour
// timestamps plus an empty-text cue that renderers must skip.
func sampleCues() []subtitle.Cue {
	return []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1500, Text: "Hello world"},
		{Index: 2, StartMS: 1500, EndMS: 3250, Text: "Second line"},
		{Index: 3, StartMS: 3250, EndMS: 4000, Text: "   "}, // empty after trim: skipped
		{Index: 4, StartMS: 3661001, EndMS: 3662010, Text: "Over an hour"},
	}
}

// TestRenderVTT pins the exact WebVTT byte output: the WEBVTT header, dot
// millisecond separator, zero-padded HH:MM:SS.mmm timestamps, skipped empty
// cue, and the trailing blank line after each cue. No numeric cue indices.
func TestRenderVTT(t *testing.T) {
	got := subtitle.RenderVTT(sampleCues())
	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:01.500\nHello world\n\n" +
		"00:00:01.500 --> 00:00:03.250\nSecond line\n\n" +
		"01:01:01.001 --> 01:01:02.010\nOver an hour\n\n"
	if got != want {
		t.Fatalf("RenderVTT mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestRenderSRT pins the exact SRT byte output: contiguous 1-based indices
// (renumbered over surviving cues so the skipped empty cue leaves no gap),
// comma millisecond separator, and the blank separator line between cues.
func TestRenderSRT(t *testing.T) {
	got := subtitle.RenderSRT(sampleCues())
	want := "1\n00:00:00,000 --> 00:00:01,500\nHello world\n\n" +
		"2\n00:00:01,500 --> 00:00:03,250\nSecond line\n\n" +
		"3\n01:01:01,001 --> 01:01:02,010\nOver an hour\n\n"
	if got != want {
		t.Fatalf("RenderSRT mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestRenderEmpty pins that an all-empty cue set yields just the WEBVTT header
// for VTT and an empty document for SRT.
func TestRenderEmpty(t *testing.T) {
	if got := subtitle.RenderVTT(nil); got != "WEBVTT\n\n" {
		t.Errorf("RenderVTT(nil) = %q, want header only", got)
	}
	if got := subtitle.RenderSRT(nil); got != "" {
		t.Errorf("RenderSRT(nil) = %q, want empty", got)
	}
	onlyEmpty := []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 1000, Text: "  "}}
	if got := subtitle.RenderSRT(onlyEmpty); got != "" {
		t.Errorf("RenderSRT(only-empty) = %q, want empty", got)
	}
}

// TestBuildCues pins the cue builder: only time spans contribute, chunks are
// reordered by span start, empty-text chunks are dropped, indices are
// 1-based and contiguous, and negative/inverted spans are clamped.
func TestBuildCues(t *testing.T) {
	chunks := []subtitle.TranscriptChunk{
		{Text: "second", Span: model.Span{Kind: "time", StartMS: 2000, EndMS: 3000}},
		{Text: "first", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 2000}},
		{Text: "  ", Span: model.Span{Kind: "time", StartMS: 3000, EndMS: 4000}}, // empty
		{Text: "no timing", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}},
		{Text: "clamped", Span: model.Span{Kind: "time", StartMS: -50, EndMS: -100}},
	}
	cues := subtitle.BuildCues(chunks)
	if len(cues) != 3 {
		t.Fatalf("BuildCues len = %d, want 3 (cues=%+v)", len(cues), cues)
	}
	// Ordered by start: clamped(-50 -> 0), first(0), second(2000). Both clamped
	// and first start at 0; stable order by end keeps clamped (end 0) first.
	if cues[0].Text != "clamped" || cues[0].StartMS != 0 || cues[0].EndMS != 0 {
		t.Errorf("cue0 = %+v, want clamped at 0-0", cues[0])
	}
	if cues[1].Text != "first" || cues[1].StartMS != 0 {
		t.Errorf("cue1 = %+v, want first at start 0", cues[1])
	}
	if cues[2].Text != "second" || cues[2].StartMS != 2000 {
		t.Errorf("cue2 = %+v, want second at start 2000", cues[2])
	}
	for i, c := range cues {
		if c.Index != i+1 {
			t.Errorf("cue %d has Index %d, want %d", i, c.Index, i+1)
		}
	}
}

// TestBuildAndRenderRoundTrip feeds synthetic transcript chunks through the
// builder into both renderers and asserts the rendered documents reflect the
// playback-ordered cues. This is the renderer-side counterpart to the
// sidecar-ingestion round trip (#253).
func TestBuildAndRenderRoundTrip(t *testing.T) {
	chunks := []subtitle.TranscriptChunk{
		{Text: "line B", Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}},
		{Text: "line A", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1000}},
	}
	cues := subtitle.BuildCues(chunks)

	wantVTT := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:01.000\nline A\n\n" +
		"00:00:01.000 --> 00:00:02.000\nline B\n\n"
	if got := subtitle.RenderVTT(cues); got != wantVTT {
		t.Errorf("round-trip VTT:\n got: %q\nwant: %q", got, wantVTT)
	}

	wantSRT := "1\n00:00:00,000 --> 00:00:01,000\nline A\n\n" +
		"2\n00:00:01,000 --> 00:00:02,000\nline B\n\n"
	if got := subtitle.RenderSRT(cues); got != wantSRT {
		t.Errorf("round-trip SRT:\n got: %q\nwant: %q", got, wantSRT)
	}
}

// TestParseTTML_ReturnsCuesInDocumentTimeOrder guards CodeRabbit finding
// ttml.go ~37: ParseTTML flattens ParseTTMLByLang's language-sorted groups, so
// without an explicit time sort the output is language-grouped, not the document
// (time) order the doc comment promises. This bilingual TTML interleaves cues by
// time across "en"/"fr"; ParseTTML must return them in ascending StartMS order.
func TestParseTTML_ReturnsCuesInDocumentTimeOrder(t *testing.T) {
	const ttml = `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xmlns:xml="http://www.w3.org/XML/1998/namespace">
  <body>
    <div xml:lang="fr">
      <p begin="00:00:01.000" end="00:00:02.000">bonjour</p>
      <p begin="00:00:03.000" end="00:00:04.000">au revoir</p>
    </div>
    <div xml:lang="en">
      <p begin="00:00:00.000" end="00:00:01.000">hello</p>
      <p begin="00:00:02.000" end="00:00:03.000">goodbye</p>
    </div>
  </body>
</tt>`

	cues, err := subtitle.ParseTTML(ttml)
	if err != nil {
		t.Fatalf("ParseTTML: %v", err)
	}
	wantStart := []int{0, 1000, 2000, 3000}
	wantText := []string{"hello", "bonjour", "goodbye", "au revoir"}
	if len(cues) != len(wantStart) {
		t.Fatalf("expected %d cues, got %d (%+v)", len(wantStart), len(cues), cues)
	}
	for i, c := range cues {
		if c.StartMS != wantStart[i] || c.Text != wantText[i] {
			t.Fatalf("cue %d: got (start=%d, text=%q), want (start=%d, text=%q)",
				i, c.StartMS, c.Text, wantStart[i], wantText[i])
		}
		if c.Index != i+1 {
			t.Fatalf("cue %d: expected 1-based index %d, got %d", i, i+1, c.Index)
		}
	}
}

// hostileCues exercises cue text and a speaker name that, if emitted raw, would
// produce malformed/unplayable subtitles: a literal '&', a '<', a "-->" timing
// arrow, an interior blank line, and a speaker name carrying '&' and a newline.
func hostileCues() []subtitle.Cue {
	return []subtitle.Cue{{
		Index:   1,
		StartMS: 0,
		EndMS:   1000,
		Text:    "Tom & Jerry <fast>\n\n--> over there",
		Speaker: "Tom & Jerry\nNarrator",
	}}
}

// TestRenderVTT_EscapesHostileText asserts that ordinary-but-hostile transcript
// text is escaped into a well-formed WebVTT cue payload: '&'/'<' are escaped,
// no raw "-->" survives in the payload (it would be read as a timing line), and
// the interior blank line is collapsed so the cue is not terminated early.
func TestRenderVTT_EscapesHostileText(t *testing.T) {
	got := subtitle.RenderVTT(hostileCues())

	// Split header from the single cue block.
	if !strings.HasPrefix(got, "WEBVTT\n\n") {
		t.Fatalf("missing WEBVTT header: %q", got)
	}
	body := strings.TrimPrefix(got, "WEBVTT\n\n")
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected timing + payload lines, got %q", got)
	}
	timing, payload := lines[0], strings.Join(lines[1:], "\n")

	if timing != "00:00:00.000 --> 00:00:01.000" {
		t.Errorf("timing line = %q, want canonical arrow", timing)
	}
	// Voice tag escaped: '&' -> "&amp;", newline -> space, tag terminated by '>'.
	wantVoice := "<v Tom &amp; Jerry Narrator>"
	if !strings.HasPrefix(payload, wantVoice) {
		t.Fatalf("voice tag = %q, want prefix %q", payload, wantVoice)
	}
	cueText := strings.TrimPrefix(payload, wantVoice)

	if strings.Contains(cueText, "\n\n") {
		t.Errorf("cue text contains interior blank line (premature cue end): %q", cueText)
	}
	if strings.Contains(cueText, "-->") {
		t.Errorf("cue text contains raw '-->' (mistakable for a timing line): %q", cueText)
	}
	// Drop well-formed entities, then assert no raw '&' or '<' remain.
	entities := strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "")
	residual := entities.Replace(cueText)
	if strings.Contains(residual, "&") {
		t.Errorf("cue text contains raw '&' outside an entity: %q", cueText)
	}
	if strings.Contains(residual, "<") {
		t.Errorf("cue text contains raw '<': %q", cueText)
	}
}

// TestRenderSRT_NeutralizesHostileText asserts SRT cue text cannot break the
// block structure: a literal "-->" is neutralised (so it is not parsed as a
// timing line) and an interior blank line is collapsed (so the cue block is not
// terminated early, which would desync every following cue).
func TestRenderSRT_NeutralizesHostileText(t *testing.T) {
	got := subtitle.RenderSRT(hostileCues())

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// Expect: "1", timing line, then payload lines — and no blank line within.
	if len(lines) < 3 {
		t.Fatalf("expected index + timing + payload, got %q", got)
	}
	if lines[0] != "1" {
		t.Errorf("index line = %q, want \"1\"", lines[0])
	}
	if lines[1] != "00:00:00,000 --> 00:00:01,000" {
		t.Errorf("timing line = %q, want canonical arrow", lines[1])
	}
	payload := strings.Join(lines[2:], "\n")
	// strings.Split on the trailing-trimmed doc must yield no empty interior
	// line: an empty element here is a premature blank line that ends the cue.
	for i, ln := range lines {
		if ln == "" {
			t.Errorf("SRT line %d is blank (premature cue end / desync): %q", i, got)
		}
	}
	if strings.Contains(payload, "-->") {
		t.Errorf("payload contains raw '-->' (mistakable for a timing line): %q", payload)
	}
	// SRT is not markup, so '&' and '<' are left intact (only '-->' is unsafe).
	if !strings.Contains(payload, "Tom & Jerry") || !strings.Contains(payload, "<fast>") {
		t.Errorf("SRT payload unexpectedly altered non-arrow text: %q", payload)
	}
}
