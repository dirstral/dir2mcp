package tests

import (
	"encoding/json"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §8.2.2 (dir2mcp #1029): a transcript segment whose window resolved to a
// language other than the representation's records `language` in its "time"
// span's extra_json, so a chunk, a citation and the §9.5 filter can see the
// minority language. These tests pin the round trip and the compatibility rule
// that an unmarked span produces no extra_json at all.

func TestTimeSpanLanguageRoundTrip(t *testing.T) {
	in := model.Span{Kind: "time", StartMS: 1_180_000, EndMS: 1_210_000, Language: "uk"}

	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if extra == "" {
		t.Fatal("a time span with its own language must produce non-empty extra_json")
	}
	var decoded struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal([]byte(extra), &decoded); err != nil {
		t.Fatalf("extra_json not valid JSON: %v (%s)", err, extra)
	}
	if decoded.Language != "uk" {
		t.Fatalf("extra_json language = %q, want uk (%s)", decoded.Language, extra)
	}

	out := store.SpanFromRow(kind, start, end, extra)
	if out.Kind != "time" || out.StartMS != 1_180_000 || out.EndMS != 1_210_000 {
		t.Fatalf("round-trip lost time bounds: %+v", out)
	}
	if out.Language != "uk" {
		t.Errorf("round-trip language = %q, want uk", out.Language)
	}
}

// TestTimeSpanLanguageCoexistsWithSpeakerAndWords confirms the language shares
// the extra_json object with the fields that were already there.
func TestTimeSpanLanguageCoexistsWithSpeakerAndWords(t *testing.T) {
	in := model.Span{
		Kind: "time", StartMS: 0, EndMS: 2000, Speaker: "S1", Language: "ky",
		Words: []model.WordSpan{{T: 0, D: 500, W: "салам"}},
	}
	kind, start, end, extra, err := store.SpanToRow(in)
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	out := store.SpanFromRow(kind, start, end, extra)
	if out.Speaker != "S1" || out.Language != "ky" || len(out.Words) != 1 {
		t.Errorf("round trip lost a field: %+v", out)
	}
}

// TestTimeSpanNoLanguage_NoExtraJSON pins backward compatibility: a span in the
// representation's language records nothing, so every transcript indexed before
// §8.2.2 round-trips byte-identically.
func TestTimeSpanNoLanguage_NoExtraJSON(t *testing.T) {
	_, _, _, extra, err := store.SpanToRow(model.Span{Kind: "time", StartMS: 0, EndMS: 2000})
	if err != nil {
		t.Fatalf("SpanToRow: %v", err)
	}
	if extra != "" {
		t.Errorf("an unmarked time span produced extra_json %q, want empty (SQL NULL)", extra)
	}
}

// TestChunkLanguageFollowsTheSpan pins the §9.5 consequence: a chunk whose time
// span carries its own language is stored under THAT language, not the
// representation's, so the per-language filter never matches a chunk on a
// language its text is not in.
func TestChunkLanguageFollowsTheSpan(t *testing.T) {
	if got := store.ChunkLanguage([]model.Span{{Kind: "time", StartMS: 0, EndMS: 1000, Language: "uk"}}, "ru"); got != "uk" {
		t.Errorf("chunk language = %q, want the span's uk", got)
	}
	if got := store.ChunkLanguage([]model.Span{{Kind: "time", StartMS: 0, EndMS: 1000}}, "ru"); got != "ru" {
		t.Errorf("chunk language = %q, want the representation's ru when the span is unmarked", got)
	}
	if got := store.ChunkLanguage(nil, "ru"); got != "ru" {
		t.Errorf("chunk language = %q, want ru with no spans", got)
	}
}
