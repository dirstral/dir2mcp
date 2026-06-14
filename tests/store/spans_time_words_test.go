package tests

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestTimeSpanWordsRoundTrip pins that per-word timing on a "time" span survives
// SpanToRow -> SpanFromRow via the extra_json `words` array (spec §8.6.1, #252).
func TestTimeSpanWordsRoundTrip(t *testing.T) {
	in := model.Span{
		Kind:    "time",
		StartMS: 1000,
		EndMS:   4000,
		Words: []model.WordSpan{
			{T: 1000, D: 400, W: "hello"},
			{T: 1400, D: 500, W: "world"},
		},
	}

	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if kind != "time" || start != 1000 || end != 4000 {
		t.Fatalf("row = (%q,%d,%d), want (time,1000,4000)", kind, start, end)
	}
	if extra == "" {
		t.Fatal("time span with words must produce a non-empty extra_json payload")
	}

	// extra_json must use the spec shape {"words":[{"t","d","w"}]}.
	var decoded struct {
		Words []map[string]any `json:"words"`
	}
	if err := json.Unmarshal([]byte(extra), &decoded); err != nil {
		t.Fatalf("extra_json not valid JSON: %v (%s)", err, extra)
	}
	if len(decoded.Words) != 2 {
		t.Fatalf("extra_json words = %d, want 2 (%s)", len(decoded.Words), extra)
	}
	for _, key := range []string{"t", "d", "w"} {
		if _, ok := decoded.Words[0][key]; !ok {
			t.Errorf("extra_json word missing %q key: %s", key, extra)
		}
	}

	out := store.SpanFromRow(kind, start, end, extra)
	if out.Kind != "time" || out.StartMS != 1000 || out.EndMS != 4000 {
		t.Fatalf("round-trip lost time bounds: %+v", out)
	}
	if !reflect.DeepEqual(out.Words, in.Words) {
		t.Errorf("round-trip words = %+v, want %+v", out.Words, in.Words)
	}
}

// TestTimeSpanWithoutWordsHasNoExtraJSON pins backward compatibility: a time
// span without words produces an empty extra_json (stored as SQL NULL), so
// pre-#252 transcripts round-trip unchanged.
func TestTimeSpanWithoutWordsHasNoExtraJSON(t *testing.T) {
	in := model.Span{Kind: "time", StartMS: 0, EndMS: 2000}

	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if strings.TrimSpace(extra) != "" {
		t.Fatalf("words-absent time span produced extra_json %q, want empty", extra)
	}

	out := store.SpanFromRow(kind, start, end, extra)
	if out.Kind != "time" || out.StartMS != 0 || out.EndMS != 2000 || out.Words != nil {
		t.Fatalf("round-trip = %+v, want clean time span with nil words", out)
	}
}

// TestTimeSpanMalformedWordsDegrade pins that a malformed extra_json payload on
// a time span degrades to no word timing rather than failing the read.
func TestTimeSpanMalformedWordsDegrade(t *testing.T) {
	out := store.SpanFromRow("time", 0, 1000, "{not valid json")
	if out.Kind != "time" || out.Words != nil {
		t.Fatalf("malformed words did not degrade cleanly: %+v", out)
	}
}
