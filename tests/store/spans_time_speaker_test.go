package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestTimeSpanSpeakerRoundTrip pins that diarized speaker attribution on a "time"
// span survives SpanToRow -> SpanFromRow via the extra_json speaker/speaker_label
// fields (SPEC §8.6.8).
func TestTimeSpanSpeakerRoundTrip(t *testing.T) {
	in := model.Span{Kind: "time", StartMS: 1000, EndMS: 4000, Speaker: "S2", SpeakerLabel: "Guest"}

	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if extra == "" {
		t.Fatal("time span with a speaker must produce non-empty extra_json")
	}
	var decoded struct {
		Speaker      string `json:"speaker"`
		SpeakerLabel string `json:"speaker_label"`
	}
	if err := json.Unmarshal([]byte(extra), &decoded); err != nil {
		t.Fatalf("extra_json not valid JSON: %v (%s)", err, extra)
	}
	if decoded.Speaker != "S2" || decoded.SpeakerLabel != "Guest" {
		t.Fatalf("extra_json speaker = %q/%q, want S2/Guest (%s)", decoded.Speaker, decoded.SpeakerLabel, extra)
	}

	out := store.SpanFromRow(kind, start, end, extra)
	if out.Kind != "time" || out.StartMS != 1000 || out.EndMS != 4000 {
		t.Fatalf("round-trip lost time bounds: %+v", out)
	}
	if out.Speaker != "S2" || out.SpeakerLabel != "Guest" {
		t.Errorf("round-trip speaker = %q/%q, want S2/Guest", out.Speaker, out.SpeakerLabel)
	}
}

// TestTimeSpanSpeakerAndWordsCoexist confirms speaker attribution and per-word
// timing share the same extra_json object without clobbering each other.
func TestTimeSpanSpeakerAndWordsCoexist(t *testing.T) {
	in := model.Span{
		Kind: "time", StartMS: 0, EndMS: 2000, Speaker: "S1", SpeakerLabel: "Host",
		Words: []model.WordSpan{{T: 0, D: 500, W: "hello"}},
	}
	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	out := store.SpanFromRow(kind, start, end, extra)
	if out.Speaker != "S1" || out.SpeakerLabel != "Host" {
		t.Errorf("speaker lost: %q/%q", out.Speaker, out.SpeakerLabel)
	}
	if len(out.Words) != 1 || out.Words[0].W != "hello" {
		t.Errorf("words lost: %+v", out.Words)
	}
}

// TestTimeSpanNoSpeaker_NoExtraJSON pins backward compatibility: a time span with
// no speaker and no words produces an empty extra_json (SQL NULL), so a
// non-diarized transcript round-trips byte-identically to before.
func TestTimeSpanNoSpeaker_NoExtraJSON(t *testing.T) {
	in := model.Span{Kind: "time", StartMS: 0, EndMS: 2000}
	_, _, _, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if strings.TrimSpace(extra) != "" {
		t.Fatalf("non-diarized time span produced extra_json %q, want empty", extra)
	}
}

// TestTimeSpanLabelWithoutID_Dropped confirms a speaker_label with no stable id
// is dropped (an id is the canonical attribution; a bare label is not valid).
func TestTimeSpanLabelWithoutID_Dropped(t *testing.T) {
	in := model.Span{Kind: "time", StartMS: 0, EndMS: 1000, SpeakerLabel: "Dangling"}
	_, _, _, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if strings.Contains(extra, "Dangling") {
		t.Fatalf("speaker_label without an id must be dropped, got extra_json %q", extra)
	}
}
