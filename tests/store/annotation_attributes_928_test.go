package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §9.10 (spec 0.59.0): an annotation's attributes ride in the "time"
// span's extra_json alongside entities/event, and MUST be recoverable per
// annotation. These pin the round trip, since a client can only be shown the
// scope it filtered by if the values come back out of the store.

func TestAnnotationAttributesRoundTrip(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "game.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 20300, EndMS: 28300,
		Entities: []string{"player:x"}, Event: "pitch",
		Attributes: map[string]string{"inning": "8", "half": "bottom"},
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Attributes["inning"] != "8" || span.Attributes["half"] != "bottom" || len(span.Attributes) != 2 {
		t.Fatalf("attributes did not round-trip: %v", span.Attributes)
	}
	// The neighbours stored beside them must be undisturbed.
	if span.Event != "pitch" || len(span.Entities) != 1 {
		t.Fatalf("attribution disturbed by attributes: %+v", span)
	}
}

// TestASpanWithNoAttributesStoresNone: optional means absent, not empty. A
// span without attributes must read back nil, and a transcript span must be
// untouched by the new key, exactly as for sources.
func TestASpanWithNoAttributesStoresNone(t *testing.T) {
	st := newAnnotationStore(t)
	annotation := annotationChunk(t, st, "plain.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1, Event: "pitch",
	})
	transcript := annotationChunk(t, st, "talk.mp3", "transcript", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1000, Speaker: "S1",
	})
	for name, chunkID := range map[string]int64{"annotation": annotation, "transcript": transcript} {
		_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
		if err != nil {
			t.Fatalf("ChunkMediaSpanByID(%s): %v", name, err)
		}
		if len(span.Attributes) != 0 {
			t.Fatalf("a %s span gained attributes: %+v", name, span)
		}
	}
}

// TestBlankAttributePairsAreNormalised: a backend that pads a key or value must
// not store a blank that no filter value could ever match.
func TestBlankAttributePairsAreNormalised(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "pad.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 0, EndMS: 1,
		Attributes: map[string]string{" inning ": " 8 ", "": "x", "half": "  "},
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Attributes["inning"] != "8" || len(span.Attributes) != 1 {
		t.Fatalf("normalization failed: %v", span.Attributes)
	}
}
