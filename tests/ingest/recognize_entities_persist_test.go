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

// TestDelimiterBearingEntityIdsCannotCollide is the regression for an ambiguous
// hash encoding. Entity ids are opaque backend-declared tokens, so a delimiter
// can legitimately appear inside one. Joining with commas encoded ["a,b","c"]
// and ["a","b,c"] identically, so two genuinely different attributions produced
// the same derivation input and the representation would NOT be re-derived —
// the precise failure the hash exists to prevent.
func TestDelimiterBearingEntityIdsCannotCollide(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{"comma inside an id", []string{"a,b", "c"}, []string{"a", "b,c"}},
		{"pipe inside an id", []string{"a|b"}, []string{"a", "b"}},
		{"colon inside an id", []string{"team:x", "y"}, []string{"team", "x:y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ha := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				annotation("same text", "pitch", tc.a, 1, 2),
			})
			_, hb := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				annotation("same text", "pitch", tc.b, 1, 2),
			})
			if ha == hb {
				t.Fatalf("%v and %v hash identically: %q", tc.a, tc.b, ha)
			}
		})
	}
}

// TestNormalisationMakesTheHashAgreeWithWhatIsStored: a blank or repeated id is
// dropped before persistence, so it must also be dropped before hashing.
// Otherwise a backend that emits a stray blank id re-derives a representation
// whose stored attribution is byte-identical — work for no change.
func TestNormalisationMakesTheHashAgreeWithWhatIsStored(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{"a blank id is not a difference", []string{"a", ""}, []string{"a"}},
		{"whitespace is trimmed", []string{" a "}, []string{"a"}},
		{"a repeat is not a difference", []string{"a", "a"}, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ha := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				annotation("same text", "pitch", tc.a, 1, 2),
			})
			_, hb := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				annotation("same text", "pitch", tc.b, 1, 2),
			})
			if ha != hb {
				t.Fatalf("%v and %v hash differently (%q vs %q) but store identically", tc.a, tc.b, ha, hb)
			}
		})
	}
}

// TestTextAndEventCannotBleedIntoEachOther: the same ambiguity applies to the
// scalar fields, where a delimiter in the annotation text could otherwise
// impersonate the start of the event field.
func TestTextAndEventCannotBleedIntoEachOther(t *testing.T) {
	_, ha := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		annotation("text|pitch", "", nil, 1, 2),
	})
	_, hb := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		annotation("text", "pitch", nil, 1, 2),
	})
	if ha == hb {
		t.Fatalf("a delimiter in the text impersonated the event field: %q", ha)
	}
}
