package tests

import (
	"reflect"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// The mapping of SPEC §7.11.4 at the unit level. The end-to-end form of
// conformance case 2 is in tests/ingest.

func buildRecord(t *testing.T, line string, duration editorial.Duration) editorial.Built {
	t.Helper()
	rec, err := editorial.ParseRecord([]byte(line), editorial.KeyStem, "example-catalogue")
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	return editorial.Build(rec, duration, editorial.Folds{})
}

func TestBuild_SegmentRanges(t *testing.T) {
	built := buildRecord(t, `{"stem": "a", "segments": [
		{"t": "00:02:30", "text": "третий"},
		{"t": "00:00:00", "text": "первый"},
		{"t": "00:01:00", "text": "второй"},
		{"t": "00:03:10", "text": "после конца"}]}`, editorial.Duration{MS: 180000, Known: true})
	type rng struct{ start, end int64 }
	var got []rng
	for _, a := range built.Annotations {
		got = append(got, rng{a.StartMS, a.EndMS})
	}
	want := []rng{{0, 60000}, {60000, 150000}, {150000, 180000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segment ranges = %v, want %v", got, want)
	}
	if built.SegmentsDropped != 1 {
		t.Fatalf("segments_dropped = %d, want 1", built.SegmentsDropped)
	}
}

func TestBuild_UnknownDurationLastSegmentIsZeroLength(t *testing.T) {
	built := buildRecord(t, `{"stem": "a", "people": [{"name": "Пётр Ёлкин"}], "segments": [
		{"t": 0, "text": "первый"}, {"t": 0, "text": "тоже первый"}, {"t": 30, "text": "последний"}]}`, editorial.Duration{})
	person := built.Annotations[0]
	if person.SpanKind != editorial.SpanDocument {
		t.Fatalf("person span kind = %q with no duration, want document", person.SpanKind)
	}
	segs := built.Annotations[1:]
	if segs[0].EndMS != 30000 || segs[1].EndMS != 30000 {
		t.Fatalf("segments with the same start end at %d and %d, want both 30000", segs[0].EndMS, segs[1].EndMS)
	}
	if last := segs[2]; last.StartMS != 30000 || last.EndMS != 30000 {
		t.Fatalf("last segment = [%d, %d], want the zero-length [30000, 30000]", last.StartMS, last.EndMS)
	}
}

// Many segments with one start share one end and cost linear time: 100,000
// equal starts would take billions of steps with a forward scan per segment.
func TestBuild_ManyEqualStarts(t *testing.T) {
	const n = 100000
	segs := make([]editorial.Segment, 0, n+1)
	for i := 0; i < n; i++ {
		segs = append(segs, editorial.Segment{StartMS: 0, Text: "x"})
	}
	segs = append(segs, editorial.Segment{StartMS: 5000, Text: "y"})
	rec := editorial.Record{Key: "a", Segments: segs}
	start := time.Now()
	built := editorial.Build(rec, editorial.Duration{MS: 9000, Known: true}, editorial.Folds{})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Build took %s for %d equal starts", elapsed, n)
	}
	for i, a := range built.Annotations[:n] {
		if a.EndMS != 5000 {
			t.Fatalf("segment %d ends at %d, want 5000", i, a.EndMS)
		}
	}
	if last := built.Annotations[n]; last.EndMS != 9000 {
		t.Fatalf("the last segment ends at %d, want the duration 9000", last.EndMS)
	}
	unknown := editorial.Build(editorial.Record{Key: "a", Segments: segs[:3]}, editorial.Duration{}, editorial.Folds{})
	for _, a := range unknown.Annotations {
		if a.EndMS != 0 {
			t.Fatalf("an equal-start last group with no duration ends at %d, want its own start 0", a.EndMS)
		}
	}
}

func TestBuild_RepeatedPersonKeepsFirstNameAndJoinsRoles(t *testing.T) {
	built := buildRecord(t, `{"stem": "a", "people": [
		{"name": "Пётр Ёлкин", "role": "инженер"},
		{"name": "ПЁТР  ЁЛКИН", "role": ""},
		{"name": "пётр ёлкин", "role": "автор"}]}`, editorial.Duration{MS: 1000, Known: true})
	if len(built.Annotations) != 1 {
		t.Fatalf("annotations = %d, want one per person id", len(built.Annotations))
	}
	a := built.Annotations[0]
	if a.Text != "Пётр Ёлкин, инженер; автор" {
		t.Fatalf("chunk text = %q", a.Text)
	}
	if a.Attributes["display_name"] != "Пётр Ёлкин" || a.Attributes["role"] != "инженер; автор" {
		t.Fatalf("attributes = %v", a.Attributes)
	}
	if !reflect.DeepEqual(a.Entities, []string{"пётр ёлкин"}) || a.Event != editorial.EventAppears {
		t.Fatalf("entities/event = %v / %q", a.Entities, a.Event)
	}
	if a.SpanKind != editorial.SpanTime || a.StartMS != 0 || a.EndMS != 1000 {
		t.Fatalf("span = %s [%d, %d], want time [0, 1000]", a.SpanKind, a.StartMS, a.EndMS)
	}
}

func TestBuild_DescriptionJoinsHeaderTopicsAndText(t *testing.T) {
	built := buildRecord(t, `{"stem": "a", "header": "Пример передачи", "topics": ["погода", " ", "город"], "description": "Полный текст."}`,
		editorial.Duration{})
	want := "Пример передачи\n\nпогода\nгород\n\nПолный текст."
	if built.Description != want {
		t.Fatalf("description = %q, want %q", built.Description, want)
	}
}

func TestRecord_NoContent(t *testing.T) {
	rec, err := editorial.ParseRecord([]byte(`{"stem": "a", "topics": [" "], "content_id": "C-1001"}`), editorial.KeyStem, "s")
	if err != nil {
		t.Fatal(err)
	}
	if rec.HasContent() {
		t.Fatal("a record with only blank topics and a provenance field has content")
	}
}

func TestCitationLabel(t *testing.T) {
	if got := editorial.CitationLabel("example-catalogue", "1.25"); got != "editor annotation (example-catalogue, match 1.25)" {
		t.Fatalf("label = %q", got)
	}
	if got := editorial.CitationLabel("example-catalogue", ""); got != "editor annotation (example-catalogue)" {
		t.Fatalf("label without score = %q", got)
	}
}
