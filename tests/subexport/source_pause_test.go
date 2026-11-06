package tests

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// SPEC §8.6.3 (0.87.0): the export path pairs a chat translation with its
// source transcript, so broadcast cues of the translation follow the source
// speech.

// pauseStore is an in-memory subexport.Store: one document with a source
// transcript (rep 1, ru, with word timings and a 5.1 s pause) and its chat
// translation (rep 2, en, no word timings).
type pauseStore struct {
	reps   []store.TranscriptRepresentation
	chunks map[int64][]store.TranscriptSpanChunk
}

func (s pauseStore) TranscriptRepresentations(_ context.Context, relPath string) ([]store.TranscriptRepresentation, error) {
	if relPath != "talk.mp4" {
		return nil, os.ErrNotExist
	}
	return s.reps, nil
}

func (s pauseStore) TranscriptSpanChunks(_ context.Context, repID int64) ([]store.TranscriptSpanChunk, error) {
	return s.chunks[repID], nil
}

func newPauseStore() pauseStore {
	return pauseStore{
		reps: []store.TranscriptRepresentation{
			{RepID: 1, MetaJSON: `{"source":"stt","language":"ru"}`},
			{RepID: 2, MetaJSON: `{"source":"translation","language":"en","source_language":"ru","span_rule":"source-spoken"}`},
		},
		chunks: map[int64][]store.TranscriptSpanChunk{
			1: {{Text: "odin dva tri chetyre pyat shest", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 8400, Words: []model.WordSpan{
				{T: 0, D: 400, W: "odin"}, {T: 500, D: 400, W: "dva"}, {T: 1000, D: 900, W: "tri"},
				{T: 7000, D: 400, W: "chetyre"}, {T: 7500, D: 400, W: "pyat"}, {T: 8000, D: 400, W: "shest"},
			}}}},
			2: {{Text: "one two three four five six", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 8400}}},
		},
	}
}

func broadcastCues(t *testing.T, pauseMS int) []subtitle.Cue {
	t.Helper()
	cfg := config.Default()
	cfg.MediaSubtitlesSegmentation = "broadcast"
	cfg.MediaSubtitlesSourcePauseMS = pauseMS
	r, err := subexport.NewRenderer(cfg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	cues, lang, err := r.Cues(context.Background(), newPauseStore(), "talk.mp4", "en", "broadcast")
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if lang != "en" || len(cues) == 0 {
		t.Fatalf("Cues returned lang %q and %d cues, want en and at least one cue", lang, len(cues))
	}
	return cues
}

// TestRendererTranslationFollowsSourcePause pins the wiring: the renderer finds
// the source transcript of the translation and no exported cue covers the
// source pause (1900, 7000).
func TestRendererTranslationFollowsSourcePause(t *testing.T) {
	cues := broadcastCues(t, config.DefaultMediaSubtitlesSourcePauseMS)
	if len(cues) < 2 {
		t.Fatalf("want a cue per spoken run, got %d: %+v", len(cues), cues)
	}
	for _, c := range cues {
		if c.StartMS < 7000 && c.EndMS > 1900 {
			t.Errorf("exported cue %+v covers the source pause (1900, 7000)", c)
		}
	}
}

// TestRendererSourcePauseOffMatchesReflow pins that source_pause_ms 0 exports
// exactly the per-segment reflow of the translation.
func TestRendererSourcePauseOffMatchesReflow(t *testing.T) {
	got := broadcastCues(t, 0)
	chunks := []subtitle.TranscriptChunk{{Text: "one two three four five six", Span: model.Span{Kind: "time", StartMS: 0, EndMS: 8400}}}
	want := subexport.BuildCuesForSegmentation(chunks, "broadcast", true)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("source_pause_ms 0 changed the export:\n got %+v\nwant %+v", got, want)
	}
}

// TestBuildCuesWithSourceChunkUnchanged pins that the chunk builder ignores the
// source: one cue per stored segment, as before.
func TestBuildCuesWithSourceChunkUnchanged(t *testing.T) {
	st := newPauseStore()
	tr := []subtitle.TranscriptChunk{{Text: st.chunks[2][0].Text, Span: st.chunks[2][0].Span}}
	src := []subtitle.TranscriptChunk{{Text: st.chunks[1][0].Text, Span: st.chunks[1][0].Span}}
	got := subexport.BuildCuesWithSource(tr, "chunk", true, src, 2000)
	if want := subtitle.BuildCues(tr); len(want) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("chunk builder changed: got %+v, want %+v", got, want)
	}
}

// TestSourceRepForPicksSourceTranscript pins the pairing: the translation's
// source is the non-translation transcript of the same track in its
// source_language, and there is none when only translations exist.
func TestSourceRepForPicksSourceTranscript(t *testing.T) {
	tr := store.TranscriptRepresentation{RepID: 9, MetaJSON: `{"source":"translation","language":"en","source_language":"ru"}`}
	reps := []store.TranscriptRepresentation{
		{RepID: 3, MetaJSON: `{"source":"sidecar","language":"de"}`},
		{RepID: 4, MetaJSON: `{"source":"stt","language":"ru","track":1}`},
		{RepID: 5, MetaJSON: `{"source":"stt","language":"ru"}`},
		tr,
	}
	got, ok := subexport.SourceRepFor(reps, tr)
	if !ok || got.RepID != 5 {
		t.Fatalf("SourceRepFor = %+v, %v; want rep 5 (ru, track 0)", got, ok)
	}
	if got, ok := subexport.SourceRepFor([]store.TranscriptRepresentation{tr}, tr); ok {
		t.Fatalf("SourceRepFor with only a translation = %+v, want no source", got)
	}
}
