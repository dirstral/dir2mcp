package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Spec 0.102.0 (SPEC §8.9.4, df-005): a recognition span stores `derivation`
// only when it is "generated". These pin the round trip and that an observed
// span stores nothing new.

func TestAnnotationDerivationRoundTrip(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "captions.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 1000, EndMS: 2000, Event: "caption",
		Derivation: model.DerivationGenerated,
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Derivation != model.DerivationGenerated {
		t.Fatalf("derivation did not round-trip: %+v", span)
	}
	if span.Event != "caption" {
		t.Fatalf("derivation disturbed the attribution next to it: %+v", span)
	}
}

func TestDerivationAloneStillPersists(t *testing.T) {
	st := newAnnotationStore(t)
	chunkID := annotationChunk(t, st, "only-derivation.mp4", "recognition", model.Span{
		Kind: "time", StartMS: 5, EndMS: 10, Derivation: model.DerivationGenerated,
	})
	_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
	if err != nil {
		t.Fatalf("ChunkMediaSpanByID: %v", err)
	}
	if span.Derivation != model.DerivationGenerated {
		t.Fatalf("a span whose only optional field is derivation lost it: %+v", span)
	}
}

func TestObservedDerivationStoresNothing(t *testing.T) {
	st := newAnnotationStore(t)
	for _, value := range []string{"", "observed"} {
		chunkID := annotationChunk(t, st, "observed-"+value+".mp4", "recognition", model.Span{
			Kind: "time", StartMS: 0, EndMS: 1, Event: "pitch", Derivation: value,
		})
		_, _, span, err := st.ChunkMediaSpanByID(context.Background(), chunkID)
		if err != nil {
			t.Fatalf("ChunkMediaSpanByID: %v", err)
		}
		if span.Derivation != "" {
			t.Fatalf("derivation %q read back as %q, want absent (absent means observed)", value, span.Derivation)
		}
	}
}
