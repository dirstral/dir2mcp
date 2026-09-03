package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §9.10 (spec 0.59.0, design 0006): an annotation MAY carry flat
// producer-defined key/value scopes, and they MUST survive ingestion per
// annotation. The measured motivation: the pilot's recognizer HAS the inning
// as structured feed data and the wire contract gave it nowhere to go, so
// "what happened in the 8th inning" could be preferred but never required.

func attributed(text string, attrs map[string]string, start, end int) model.RecognizedAnnotation {
	return model.RecognizedAnnotation{
		StartMS: start, EndMS: end, Text: text,
		Event: "pitch", Entities: []string{"player:x"}, Attributes: attrs,
	}
}

func TestAttributesReachTheChunkSpan(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		attributed("Pitch: X to Y (bottom of the 8th)",
			map[string]string{"inning": "8", "half": "bottom"}, 1000, 2000),
	})
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	got := segments[0].Span.Attributes
	if got["inning"] != "8" || got["half"] != "bottom" || len(got) != 2 {
		t.Fatalf("span.Attributes = %v, want inning=8 half=bottom", got)
	}
}

// TestHashUnchangedWithoutAttributes is the migration-safety property, and it
// is the one that can silently cost a corpus: the attributes group joins the
// derivation hash APPENDED ONLY WHEN PRESENT (like sources in #861), so an
// annotation without attributes hashes byte-identically to before this field
// existed, and no existing corpus re-runs its recognition backend.
func TestHashUnchangedWithoutAttributes(t *testing.T) {
	base := []model.RecognizedAnnotation{
		attributed("Pitch: X to Y", nil, 1000, 2000),
	}
	_, hashWithout := ingest.RecognitionSegments(base)
	withNil := []model.RecognizedAnnotation{
		attributed("Pitch: X to Y", map[string]string{}, 1000, 2000),
	}
	_, hashEmpty := ingest.RecognitionSegments(withNil)
	if hashWithout != hashEmpty {
		t.Fatalf("an EMPTY attributes map changed the hash input:\n%q\nvs\n%q", hashWithout, hashEmpty)
	}
	// And whitespace-only pairs normalize away entirely, so they cannot change
	// the hash either.
	withBlank := []model.RecognizedAnnotation{
		attributed("Pitch: X to Y", map[string]string{"  ": " ", "inning": "  "}, 1000, 2000),
	}
	_, hashBlank := ingest.RecognitionSegments(withBlank)
	if hashWithout != hashBlank {
		t.Fatalf("blank attribute pairs changed the hash input")
	}
}

// TestChangedAttributesRederive: the other direction. A backend that changes
// an annotation's attributes has produced different content, and the
// representation must re-derive (§8.6.7), exactly as for entities.
func TestChangedAttributesRederive(t *testing.T) {
	_, h8 := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		attributed("Pitch: X to Y", map[string]string{"inning": "8"}, 1000, 2000),
	})
	_, h9 := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		attributed("Pitch: X to Y", map[string]string{"inning": "9"}, 1000, 2000),
	})
	if h8 == h9 {
		t.Fatalf("different attributes produced the same derivation hash")
	}
}

// TestAttributeHashIsMapOrderIndependent: maps iterate in random order, and an
// order-only difference must not flap the rep_hash and force a spurious
// re-derivation (the same property #861 pinned for the response order).
func TestAttributeHashIsMapOrderIndependent(t *testing.T) {
	attrs := map[string]string{"inning": "8", "half": "bottom", "outs": "2"}
	_, first := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		attributed("Pitch: X to Y", attrs, 1000, 2000),
	})
	for i := 0; i < 20; i++ {
		rebuilt := map[string]string{"outs": "2", "half": "bottom", "inning": "8"}
		_, again := ingest.RecognitionSegments([]model.RecognizedAnnotation{
			attributed("Pitch: X to Y", rebuilt, 1000, 2000),
		})
		if first != again {
			t.Fatalf("hash flapped on map iteration order (run %d)", i)
		}
	}
}

// TestReservedPrefixDropsTheAnnotation: a producer MUST NOT emit dir2mcp: keys
// (§9.10), and the established malformed-annotation behaviour is to DROP the
// offender while its siblings proceed (design 0004 §5, like a reversed span).
// Stripping the key instead would store an annotation the producer never sent.
func TestReservedPrefixDropsTheAnnotation(t *testing.T) {
	segments, _ := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		attributed("kept", map[string]string{"inning": "8"}, 1000, 2000),
		attributed("dropped", map[string]string{"dir2mcp:internal": "x"}, 3000, 4000),
		attributed("also kept", nil, 5000, 6000),
	})
	if len(segments) != 2 {
		t.Fatalf("expected the reserved-key annotation dropped and 2 kept, got %d", len(segments))
	}
	for _, seg := range segments {
		if seg.Text == "dropped" {
			t.Fatalf("the reserved-key annotation was stored")
		}
	}
}

// TestAttributesCollideProofHashGrouping: the attributes group is tagged so it
// can never be read as a sources group. Two annotations, one with two sources
// and one with two attributes, must hash differently even when every
// length-prefixed token happens to align.
func TestAttributesCollideProofHashGrouping(t *testing.T) {
	_, withSources := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		{StartMS: 1000, EndMS: 2000, Text: "t", Event: "e",
			Sources: []string{"aa", "bb"}},
	})
	_, withAttrs := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		{StartMS: 1000, EndMS: 2000, Text: "t", Event: "e",
			Attributes: map[string]string{"aa": "bb"}},
	})
	if withSources == withAttrs {
		t.Fatalf("a sources group and an attributes group hashed identically")
	}
}

// TestHashInputGolden pins the exact hash input for an attribute-less response,
// byte for byte, as produced by main BEFORE attributes existed (verified by
// running both versions side by side on 2026-09-02). Internal-consistency
// tests cannot catch a change that moves both sides together; this golden can.
// If this fails, every existing corpus re-runs its recognition backend on next
// scan, so a deliberate format change must be reasoned about, not just made.
func TestHashInputGolden(t *testing.T) {
	_, hash := ingest.RecognitionSegments([]model.RecognizedAnnotation{
		{StartMS: 1000, EndMS: 2000, Text: "Pitch: X to Y", Event: "pitch",
			Entities: []string{"player:x"}, Sources: []string{"playbyplay"}},
		{StartMS: 3000, EndMS: 4000, Text: "plain"},
	})
	const golden = "1000|2000|13:Pitch: X to Y|5:pitch|1|8:player:x|1|10:playbyplay\n" +
		"3000|4000|5:plain|0:|0\n"
	if hash != golden {
		t.Fatalf("derivation hash input changed for attribute-less annotations:\ngot  %q\nwant %q", hash, golden)
	}
}
