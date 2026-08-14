package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// A recognition annotation's recognizer tags ride in the "time" span's
// extra_json alongside `entities`/`event` (df-005 0.3.0 `sources`). These pin
// the round trip: a client can only be told who spoke if the value comes back
// out of the store.

func TestAnnotationSourcesRoundTrip(t *testing.T) {
	st := newAnnotationStore(t)
	sources := []string{"playbyplay", "scorebug", "face"}
	chunkID := annotationChunk(t, st, "game.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 20300, EndMS: 28300,
		Entities: []string{"player:robbie-ray"}, Event: "pitch",
		Sources: sources,
	})

	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if strings.Join(span.Sources, ",") != strings.Join(sources, ",") {
		// Order is the producer's: df-005 keeps the tags as the backend named them.
		t.Fatalf("sources did not round-trip: %v, want %v", span.Sources, sources)
	}
	// The provenance must not disturb the attribution stored next to it.
	if span.Event != "pitch" || strings.Join(span.Entities, ",") != "player:robbie-ray" {
		t.Fatalf("attribution disturbed by sources: %+v", span)
	}
}

// TestASpanWithNoSourcesStoresNone: `sources` is optional, so a span without it
// must read back empty rather than as an empty list, and a transcript span must
// be untouched by the new key.
func TestASpanWithNoSourcesStoresNone(t *testing.T) {
	st := newAnnotationStore(t)
	annotation := annotationChunk(t, st, "plain.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1,
		Entities: []string{"player:a"}, Event: "pitch",
	})
	transcript := annotationChunk(t, st, "talk.mp3", "transcript", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1000, Speaker: "S1", SpeakerLabel: "Host",
	})
	for name, chunkID := range map[string]int64{"annotation": annotation, "transcript": transcript} {
		_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
		if err != nil {
			t.Fatalf("ChunkMediaSpanByID(%s): %v", name, err)
		}
		if len(span.Sources) != 0 {
			t.Fatalf("a %s span gained recognizer provenance: %+v", name, span)
		}
	}
}

// TestDuplicateAndBlankSourceTagsAreNormalised: a backend that repeats a tag, or
// pads one, must not inflate the stored provenance or store a blank tag that
// renders as an empty recognizer name.
func TestDuplicateAndBlankSourceTagsAreNormalised(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "dupes.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1,
		Sources: []string{" scorebug ", "scorebug", "", "   ", "face"},
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if strings.Join(span.Sources, ",") != "scorebug,face" {
		t.Fatalf("sources = %v, want the trimmed de-duplicated pair", span.Sources)
	}
}

// TestSourcesAloneStillPersist: the extra_json object is written only when some
// optional field is present. A span whose ONLY optional field is `sources` must
// still write the object, or the provenance is dropped at the last step.
func TestSourcesAloneStillPersist(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "only-sources.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 5, EndMS: 10, Sources: []string{"scorebug"},
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if strings.Join(span.Sources, ",") != "scorebug" {
		t.Fatalf("sources = %v, want [scorebug]", span.Sources)
	}
	if span.StartMS != 5 || span.EndMS != 10 {
		t.Fatalf("span bounds disturbed: %+v", span)
	}
}
