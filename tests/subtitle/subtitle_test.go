package tests

import (
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
