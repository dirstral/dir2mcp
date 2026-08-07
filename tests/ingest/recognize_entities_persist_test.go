package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// dirstral-spec design 0004 §7: a recognition annotation's entity ids and its
// `event` MUST survive ingestion.
//
// They did not. `recognitionSegments` rebuilt each annotation as
// {startMS, endMS, text}, and `RecognizedAnnotation.Entities` appeared exactly
// once in the whole codebase — at the line that parsed it off the wire. The
// backend was required to compute attribution that was then thrown away, which
// is why the entity filter could not be implemented against data that already
// existed end to end.

func annotation(text, event string, entities []string, start, end int) model.RecognizedAnnotation {
	return model.RecognizedAnnotation{
		StartMS: start, EndMS: end, Text: text, Event: event, Entities: entities,
	}
}

func TestAnnotationEntitiesAndEventReachTheChunkSpan(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		annotation("Pitch: Robbie Ray to Dylan Crews", "pitch",
			[]string{"player:robbie-ray", "team:san-francisco-giants"}, 20300, 28300),
	})
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	span := segments[0].Span
	if span.Event != "pitch" {
		t.Fatalf("span.Event = %q, want pitch", span.Event)
	}
	want := []string{"player:robbie-ray", "team:san-francisco-giants"}
	if len(span.Entities) != len(want) {
		t.Fatalf("span.Entities = %v, want %v", span.Entities, want)
	}
	for i, id := range want {
		if span.Entities[i] != id {
			// Order matters: the acting entity is emitted first, and the filter
			// is documented as preserving what the backend sent.
			t.Fatalf("span.Entities[%d] = %q, want %q", i, span.Entities[i], id)
		}
	}
}

// TestAnAnnotationWithNoAttributionIsUnchanged: entities and event are optional,
// so a backend that sends neither (or a non-annotation representation) produces
// exactly the span it produced before.
func TestAnAnnotationWithNoAttributionIsUnchanged(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		annotation("something happened", "", nil, 0, 1000),
	})
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if len(segments[0].Span.Entities) != 0 || segments[0].Span.Event != "" {
		t.Fatalf("an unattributed annotation gained attribution: %+v", segments[0].Span)
	}
}

// TestChangedAttributionRedervesTheRepresentation: the derivation hash must
// cover the attribution (§8.6.7). Otherwise a backend that fixes which entities
// an annotation names would leave the old, wrong attribution in place because
// the prose happens to be identical.
func TestChangedAttributionRedervesTheRepresentation(t *testing.T) {
	base := []model.RecognizedAnnotation{
		annotation("Pitch: Robbie Ray to Dylan Crews", "pitch", []string{"player:robbie-ray"}, 1, 2),
	}
	changedEntities := []model.RecognizedAnnotation{
		annotation("Pitch: Robbie Ray to Dylan Crews", "pitch",
			[]string{"player:robbie-ray", "team:san-francisco-giants"}, 1, 2),
	}
	changedEvent := []model.RecognizedAnnotation{
		annotation("Pitch: Robbie Ray to Dylan Crews", "at_bat", []string{"player:robbie-ray"}, 1, 2),
	}

	_, h0 := ingest.RecognitionSegments(base)
	_, h1 := ingest.RecognitionSegments(changedEntities)
	_, h2 := ingest.RecognitionSegments(changedEvent)

	if h0 == h1 {
		t.Fatal("adding an entity left the derivation hash input unchanged")
	}
	if h0 == h2 {
		t.Fatal("changing the event left the derivation hash input unchanged")
	}
	// And the text is still in there, so the old invalidation still works.
	if !strings.Contains(h0, "Pitch: Robbie Ray to Dylan Crews") {
		t.Fatalf("hash input no longer covers the annotation text: %q", h0)
	}
}
