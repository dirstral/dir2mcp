package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestLanguagePrimarySubtag pins the canonical primary-subtag normalization used
// by the per-language retrieval filter (SPEC §9.5): lower-cased, segment before
// the first '-' or '_', empty for a blank tag.
func TestLanguagePrimarySubtag(t *testing.T) {
	cases := map[string]string{
		"en":      "en",
		"EN":      "en",
		"en-US":   "en",
		"pt-BR":   "pt",
		"zh-Hant": "zh",
		"en_us":   "en",
		"  fr  ":  "fr",
		"":        "",
		"   ":     "",
	}
	for in, want := range cases {
		if got := model.LanguagePrimarySubtag(in); got != want {
			t.Errorf("LanguagePrimarySubtag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsValidLanguageTag pins the lenient BCP-47 validity check that decides
// INVALID_FIELD (§9.5/§14): common tags are valid; clearly malformed values are
// rejected.
func TestIsValidLanguageTag(t *testing.T) {
	valid := []string{"en", "EN", "pt-BR", "zh-Hant", "und", "es", "de-DE-1996"}
	for _, tag := range valid {
		if !model.IsValidLanguageTag(tag) {
			t.Errorf("IsValidLanguageTag(%q) = false, want true", tag)
		}
	}
	invalid := []string{"", "   ", "not a tag!", "@@@", "en-", "-en", "en--US", "123", "en-US-"}
	for _, tag := range invalid {
		if model.IsValidLanguageTag(tag) {
			t.Errorf("IsValidLanguageTag(%q) = true, want false", tag)
		}
	}
}

// TestLanguageMatchesAny pins the §9.5 matching contract: primary-subtag,
// case-insensitive, logical OR; unknown (empty recorded) never matches a
// specific filter.
func TestLanguageMatchesAny(t *testing.T) {
	cases := []struct {
		recorded  string
		requested []string
		want      bool
	}{
		{"en", []string{"en"}, true},
		{"en-US", []string{"EN"}, true},
		{"pt-BR", []string{"pt"}, true},
		{"pt-BR", []string{"es", "pt"}, true}, // OR
		{"en", []string{"pt", "es"}, false},
		{"", []string{"en"}, false},    // unknown never matches a specific filter
		{"en", []string{"und"}, false}, // und is not en
		{"", []string{"und"}, false},   // unknown is not matched even by und here
	}
	for _, tc := range cases {
		if got := model.LanguageMatchesAny(tc.recorded, tc.requested); got != tc.want {
			t.Errorf("LanguageMatchesAny(%q, %v) = %v, want %v", tc.recorded, tc.requested, got, tc.want)
		}
	}
}

// TestFilterMatch_Languages exercises the predicate through model.Filter.Match,
// the single authoritative path shared by the in-Go re-check and backend
// pushdown (HNSW/disk), to ensure language filtering composes with the existing
// predicates and that an unknown-language payload is excluded by a non-empty
// filter but kept when the filter is empty.
func TestFilterMatch_Languages(t *testing.T) {
	en := model.IndexPayload{ChunkID: 1, RelPath: "a.txt", DocType: "text", Language: "en-GB"}
	unknown := model.IndexPayload{ChunkID: 2, RelPath: "b.txt", DocType: "text", Language: ""}

	specific := model.Filter{Languages: []string{"en"}}
	if !specific.Match(en) {
		t.Error("en-GB payload must match a [en] filter (primary subtag)")
	}
	if specific.Match(unknown) {
		t.Error("unknown-language payload must NOT match a specific [en] filter")
	}

	empty := model.Filter{}
	if !empty.Match(unknown) || !empty.Match(en) {
		t.Error("an empty filter must match every payload (no language filtering)")
	}
}
