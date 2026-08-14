package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// dirstral-spec 0.52.0 (df-005 0.3.0): a recognition annotation MAY name the
// recognizers that produced it, and that provenance MUST survive ingestion.
//
// It did not. `recognizeWireResponse` decoded `sources` and `Recognize` copied
// it onto the annotation, then `recognitionSegments` rebuilt each annotation as
// {startMS, endMS, text, entities, event} and the tags stopped there. So the
// backend named the scorebug reader or the face matcher, and no client could
// ever learn which one spoke (#861).

// sourced builds one annotation that names its recognizers.
func sourced(text, event string, entities, sources []string, start, end int) model.RecognizedAnnotation {
	return model.RecognizedAnnotation{
		StartMS: start, EndMS: end, Text: text,
		Event: event, Entities: entities, Sources: sources,
	}
}

func TestAnnotationSourcesReachTheChunkSpan(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		sourced("Heliot Ramos hits a home run", "home_run",
			[]string{"player:heliot-ramos"}, []string{"playbyplay", "scorebug", "face"},
			20300, 28300),
	})
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	got := segments[0].Span.Sources
	want := []string{"playbyplay", "scorebug", "face"}
	if len(got) != len(want) {
		t.Fatalf("span.Sources = %v, want %v", got, want)
	}
	for i, tag := range want {
		if got[i] != tag {
			// Order is the producer's and df-005 keeps it: the tags come back
			// exactly as the backend named them.
			t.Fatalf("span.Sources[%d] = %q, want %q", i, got[i], tag)
		}
	}
}

// TestAnAnnotationWithNoSourcesIsUnchanged: `sources` is optional, so a backend
// that sends none produces exactly the span it produced before.
func TestAnAnnotationWithNoSourcesIsUnchanged(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		sourced("something happened", "pitch", []string{"player:a"}, nil, 0, 1000),
	})
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if len(segments[0].Span.Sources) != 0 {
		t.Fatalf("an annotation with no sources gained provenance: %+v", segments[0].Span)
	}
}

// TestChangedSourcesRederiveTheRepresentation: the derivation hash covers what
// is stored (§8.6.7), and the tags are now stored. A backend that re-attributes
// an annotation from the scorebug to the face matcher must re-derive the
// representation, not keep the old provenance because the prose still matches.
func TestChangedSourcesRederiveTheRepresentation(t *testing.T) {
	base := []model.RecognizedAnnotation{
		sourced("Ramos hits a home run", "home_run", []string{"player:a"}, []string{"scorebug"}, 1, 2),
	}
	changed := []model.RecognizedAnnotation{
		sourced("Ramos hits a home run", "home_run", []string{"player:a"}, []string{"face"}, 1, 2),
	}
	added := []model.RecognizedAnnotation{
		sourced("Ramos hits a home run", "home_run", []string{"player:a"}, []string{"scorebug", "face"}, 1, 2),
	}

	_, h0 := ingest.RecognitionSegments(base)
	_, h1 := ingest.RecognitionSegments(changed)
	_, h2 := ingest.RecognitionSegments(added)

	if h0 == h1 {
		t.Fatal("re-attributing the annotation to another recognizer left the derivation hash input unchanged")
	}
	if h0 == h2 {
		t.Fatal("adding a recognizer left the derivation hash input unchanged")
	}
}

// TestSourceTagsCannotCollideInTheHash mirrors the entity-id guard. A tag is an
// opaque backend token, so any delimiter can appear inside one and the encoding
// must stay length-prefixed.
func TestSourceTagsCannotCollideInTheHash(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{"pipe inside a tag", []string{"a|b"}, []string{"a", "b"}},
		{"colon inside a tag", []string{"feed:x", "y"}, []string{"feed", "x:y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ha := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				sourced("same text", "pitch", nil, tc.a, 1, 2),
			})
			_, hb := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				sourced("same text", "pitch", nil, tc.b, 1, 2),
			})
			if ha == hb {
				t.Fatalf("%v and %v hash identically: %q", tc.a, tc.b, ha)
			}
		})
	}
}

// TestSourceNormalisationMakesTheHashAgreeWithWhatIsStored: a blank or repeated
// tag is dropped before persistence, so it must also be dropped before hashing.
// Otherwise a backend that emits a stray blank tag re-derives a representation
// whose stored provenance is byte-identical.
func TestSourceNormalisationMakesTheHashAgreeWithWhatIsStored(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
	}{
		{"a blank tag is not a difference", []string{"scorebug", ""}, []string{"scorebug"}},
		{"whitespace is trimmed", []string{" scorebug "}, []string{"scorebug"}},
		{"a repeat is not a difference", []string{"scorebug", "scorebug"}, []string{"scorebug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ha := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				sourced("same text", "pitch", nil, tc.a, 1, 2),
			})
			_, hb := ingest.RecognitionSegments([]model.RecognizedAnnotation{
				sourced("same text", "pitch", nil, tc.b, 1, 2),
			})
			if ha != hb {
				t.Fatalf("%v and %v hash differently (%q vs %q) but store identically", tc.a, tc.b, ha, hb)
			}
		})
	}
}

// TestASourcelessAnnotationKeepsItsOldHashInput is the upgrade guard. Recognition
// is the most expensive derivation in the tree: a changed hash input re-runs the
// backend over every video in a corpus. An annotation that names no recognizer
// gains no stored provenance, so its hash input MUST stay exactly what it was
// before this field existed, and only an annotation that carries tags re-derives.
func TestASourcelessAnnotationKeepsItsOldHashInput(t *testing.T) {
	_, plain := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		sourced("same text", "pitch", nil, nil, 1, 2),
	})
	const want = "1|2|9:same text|5:pitch|0\n"
	if plain != want {
		t.Fatalf("a sourceless annotation hashes as %q, want %q: every existing corpus would re-run its recognition backend", plain, want)
	}
	_, tagged := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		sourced("same text", "pitch", nil, []string{"scorebug"}, 1, 2),
	})
	if !strings.HasPrefix(tagged, want[:len(want)-1]) || tagged == plain {
		t.Fatalf("a tagged annotation must extend the same input, got %q vs %q", tagged, plain)
	}
}
