package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestProvenance_VTTRoundTrip pins SPEC §8.6.14 provenance for WebVTT: a stamped
// render carries an intact marker, parses to exactly the cues of the unstamped
// render (the NOTE block is ignored), and any change to the body breaks it.
func TestProvenance_VTTRoundTrip(t *testing.T) {
	cues := []subtitle.Cue{{StartMS: 0, EndMS: 2000, Text: "one"}, {StartMS: 2000, EndMS: 4000, Text: "two"}}
	plain := subtitle.RenderVTT(cues)
	stamped := subtitle.StampVTT(plain)
	if stamped == plain || !strings.HasPrefix(stamped, "WEBVTT") {
		t.Fatalf("stamp must insert a marker after the header:\n%s", stamped)
	}
	p := subtitle.ReadProvenance([]byte(stamped))
	if !p.Marked || !p.Intact || len(p.SHA256) != 64 {
		t.Fatalf("stamped VTT must read as intact, got %+v", p)
	}
	a, err := subtitle.ParseVTT(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := subtitle.ParseVTT(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || a[0].Text != b[0].Text || a[1].Text != b[1].Text {
		t.Fatalf("marker changed the parsed cues: %+v vs %+v", a, b)
	}
	edited := strings.Replace(stamped, "two", "TWO", 1)
	if p := subtitle.ReadProvenance([]byte(edited)); !p.Marked || p.Intact {
		t.Fatalf("an edited body must read as marked but not intact, got %+v", p)
	}
	if p := subtitle.ReadProvenance([]byte(plain)); p.Marked {
		t.Fatalf("an unstamped VTT must read as unmarked, got %+v", p)
	}
}

// TestProvenance_TTMLRoundTrip pins the same contract for TTML, whose marker is
// an XML comment the TTML parser ignores.
func TestProvenance_TTMLRoundTrip(t *testing.T) {
	cues := []subtitle.BilingualCue{{StartMS: 0, EndMS: 2000, PrimaryLang: "ru", PrimaryText: "раз", SecondaryLang: "en", SecondaryText: "one"}}
	plain := subtitle.RenderTTML(cues, "ru")
	stamped := subtitle.StampTTML(plain)
	if p := subtitle.ReadProvenance([]byte(stamped)); !p.Marked || !p.Intact {
		t.Fatalf("stamped TTML must read as intact, got %+v", p)
	}
	a, err := subtitle.ParseTTMLByLang(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := subtitle.ParseTTMLByLang(stamped)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("marker changed the parsed languages: %d vs %d", len(a), len(b))
	}
	if p := subtitle.ReadProvenance([]byte(strings.Replace(stamped, "one", "uno", 1))); p.Intact {
		t.Fatalf("an edited TTML body must not read as intact")
	}
}

// TestProvenance_HeadProbe pins that the cheap head probe sees the marker in
// the first ProvenanceHeadBytes of a stamped file and nothing in an unmarked one.
func TestProvenance_HeadProbe(t *testing.T) {
	stamped := subtitle.StampVTT(subtitle.RenderVTT([]subtitle.Cue{{StartMS: 0, EndMS: 1000, Text: strings.Repeat("x", 2000)}}))
	head := []byte(stamped)[:subtitle.ProvenanceHeadBytes]
	if !subtitle.MayCarryProvenance(head) {
		t.Fatalf("the marker must sit within the first %d bytes", subtitle.ProvenanceHeadBytes)
	}
	if subtitle.MayCarryProvenance([]byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhi\n")) {
		t.Fatalf("an unmarked file must not match the probe")
	}
	if subtitle.StampVTT("not a vtt") != "not a vtt" || subtitle.StampTTML("<tt/>") != "<tt/>" {
		t.Fatalf("stamping must leave non-matching input unchanged")
	}
}
