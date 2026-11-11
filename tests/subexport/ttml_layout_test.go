package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
)

// SPEC §8.6.10 (0.91.0): under media.subtitles.ttml.layout=divs each language
// keeps its own cues and timings. A Whisper translation runs a second or two
// behind the source; aligning it onto source cue windows moved sentences into
// the neighbouring cue on the TV Rain archive canary.

// laggedStore holds a Russian transcript and an English translation whose
// segments start 3 s later, inside the 2.5 s alignment tolerance of the NEXT
// Russian segment.
func laggedStore() pauseStore {
	return pauseStore{
		reps: []store.TranscriptRepresentation{
			{RepID: 1, MetaJSON: `{"source":"stt","language":"ru"}`},
			{RepID: 2, MetaJSON: `{"source":"translation","language":"en","source_language":"ru"}`},
		},
		chunks: map[int64][]store.TranscriptSpanChunk{
			1: {
				{Text: "Одно не исключает другого.", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 4000}},
				{Text: "Сроки министр не раскрыл.", Span: model.Span{Kind: "time", StartMS: 4000, EndMS: 8000}},
			},
			2: {
				{Text: "One does not exclude the other.", Span: model.Span{Kind: "time", StartMS: 3000, EndMS: 6000}},
				{Text: "The minister did not disclose the terms.", Span: model.Span{Kind: "time", StartMS: 6000, EndMS: 9000}},
			},
		},
	}
}

func renderLaggedTTML(t *testing.T, layout string) string {
	t.Helper()
	cfg := config.Default()
	cfg.MediaSubtitlesTTMLEnabled = true
	cfg.MediaSubtitlesTTMLLayout = layout
	cfg.MediaSubtitlesTTMLLanguageCodes = "iso639_2"
	r, err := subexport.NewRenderer(cfg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	ttml, _, err := r.RenderTTML(context.Background(), laggedStore(), "talk.mp4", "ru", "en")
	if err != nil {
		t.Fatalf("RenderTTML: %v", err)
	}
	return ttml
}

// TestRenderTTML_DivsKeepEachLanguagesOwnTiming pins that the English <div>
// carries the translation's own cues, so each sentence stays in its own cue.
func TestRenderTTML_DivsKeepEachLanguagesOwnTiming(t *testing.T) {
	got := renderLaggedTTML(t, "divs")
	for _, want := range []string{
		`<p begin="00:00:00.000" end="00:00:04.000">Одно не исключает другого.</p>`,
		`<p begin="00:00:03.000" end="00:00:06.000">One does not exclude the other.</p>`,
		`<p begin="00:00:06.000" end="00:00:09.000">The minister did not disclose the terms.</p>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("divs TTML is missing %s:\n%s", want, got)
		}
	}
}

// TestRenderTTML_SpansStillAlign pins that the default layout, which shows both
// languages in one cue, still aligns the translation onto the source windows.
func TestRenderTTML_SpansStillAlign(t *testing.T) {
	got := renderLaggedTTML(t, "spans")
	if strings.Contains(got, `begin="00:00:03.000"`) || strings.Contains(got, `<div xml:lang`) {
		t.Fatalf("spans TTML must align onto source cue windows:\n%s", got)
	}
}
