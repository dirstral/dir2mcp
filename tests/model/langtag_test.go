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
	valid := []string{
		"en", "EN", "pt-BR", "zh-Hant", "und", "es", "de-DE-1996",
		// The `_` locale form is accepted as an alias for `-` (issue #441 item
		// 2.2): a filter is no longer rejected for a form that stored content is
		// honored under (LanguagePrimarySubtag already collapses `_`).
		"en_US", "pt_BR",
	}
	for _, tag := range valid {
		if !model.IsValidLanguageTag(tag) {
			t.Errorf("IsValidLanguageTag(%q) = false, want true", tag)
		}
	}
	invalid := []string{
		"", "   ", "not a tag!", "@@@", "en-", "-en", "en--US", "123", "en-US-",
		// A single-letter primary subtag is a BCP-47 singleton/grandfathered
		// prefix, not a filterable language (issue #441 item 2.3).
		"x", "a", "x-klingon", "en_", "_en",
	}
	for _, tag := range invalid {
		if model.IsValidLanguageTag(tag) {
			t.Errorf("IsValidLanguageTag(%q) = true, want false", tag)
		}
	}
}

// TestLanguageTagValidationConsistency pins the #441 item 2.2 fix: a filter tag
// is accepted by IsValidLanguageTag exactly when stored content in the same form
// is honored by LanguagePrimarySubtag. Before the fix, `en_US` yielded a stored
// primary subtag ("en") yet was rejected as a client filter — an asymmetry that
// silently dropped a legitimate filter.
func TestLanguageTagValidationConsistency(t *testing.T) {
	for _, tag := range []string{"en_US", "en-US", "pt_BR", "zh_Hant"} {
		primary := model.LanguagePrimarySubtag(tag)
		if primary == "" {
			t.Fatalf("LanguagePrimarySubtag(%q) unexpectedly empty", tag)
		}
		if !model.IsValidLanguageTag(tag) {
			t.Errorf("stored form %q yields primary %q but IsValidLanguageTag rejects the same filter (asymmetry, #441 2.2)", tag, primary)
		}
	}
}

// TestLanguageMatchesAnyMode_StrictUnderscore confirms the `_` locale form
// filters identically to the `-` form under strict matching (issue #441 item
// 2.2): `en_US` narrows exactly like `en-US`.
func TestLanguageMatchesAnyMode_StrictUnderscore(t *testing.T) {
	cases := []struct {
		recorded  string
		requested string
		want      bool
	}{
		{"en-US", "en_US", true},  // underscore request matches hyphen recorded
		{"en_US", "en-US", true},  // hyphen request matches underscore recorded
		{"en-GB", "en_US", false}, // still narrows region
		{"en", "en_US", false},    // bare en excluded by en_US request
	}
	for _, tc := range cases {
		got := model.LanguageMatchesAnyMode(tc.recorded, []string{tc.requested}, model.LanguageMatchStrict)
		if got != tc.want {
			t.Errorf("strict match(recorded=%q, req=%q) = %v, want %v", tc.recorded, tc.requested, got, tc.want)
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

// TestIsValidLanguageMatch pins the §9.5 match-mode validity check: absent/empty
// and the two recognized modes are valid; anything else is INVALID_FIELD.
func TestIsValidLanguageMatch(t *testing.T) {
	for _, m := range []string{"", "  ", "primary", "PRIMARY", "strict", "Strict"} {
		if !model.IsValidLanguageMatch(m) {
			t.Errorf("IsValidLanguageMatch(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"loose", "exact", "basic", "und", "prim"} {
		if model.IsValidLanguageMatch(m) {
			t.Errorf("IsValidLanguageMatch(%q) = true, want false", m)
		}
	}
}

// TestNormalizeLanguageMatch pins that only "strict" (case-insensitive) selects
// strict; everything else (incl. "", junk) degrades to the primary default.
func TestNormalizeLanguageMatch(t *testing.T) {
	cases := map[string]string{
		"":          model.LanguageMatchPrimary,
		"primary":   model.LanguageMatchPrimary,
		" PRIMARY ": model.LanguageMatchPrimary,
		"strict":    model.LanguageMatchStrict,
		" Strict ":  model.LanguageMatchStrict,
		"nonsense":  model.LanguageMatchPrimary,
	}
	for in, want := range cases {
		if got := model.NormalizeLanguageMatch(in); got != want {
			t.Errorf("NormalizeLanguageMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLanguageMatchesAnyMode_Primary confirms the mode-aware entry point matches
// LanguageMatchesAny under the default/empty/"primary"/junk modes (§9.5 default).
func TestLanguageMatchesAnyMode_Primary(t *testing.T) {
	for _, mode := range []string{"", "primary", "PRIMARY", "unknown-mode"} {
		if !model.LanguageMatchesAnyMode("pt-BR", []string{"pt"}, mode) {
			t.Errorf("mode %q: pt-BR should match [pt] under primary semantics", mode)
		}
		if !model.LanguageMatchesAnyMode("en-US", []string{"EN"}, mode) {
			t.Errorf("mode %q: en-US should match [EN] under primary semantics", mode)
		}
		if model.LanguageMatchesAnyMode("", []string{"en"}, mode) {
			t.Errorf("mode %q: unknown recorded must never match a specific filter", mode)
		}
	}
}

// TestLanguageMatchesAnyMode_Strict pins the §9.5 opt-in region/script narrowing
// (RFC 4647 Basic Filtering): a request narrows only to the precision it supplies.
func TestLanguageMatchesAnyMode_Strict(t *testing.T) {
	cases := []struct {
		recorded  string
		requested []string
		want      bool
		why       string
	}{
		// Region narrowing: pt-BR narrows away pt and pt-PT.
		{"pt-BR", []string{"pt-BR"}, true, "exact region match"},
		{"PT-br", []string{"pt-BR"}, true, "case-insensitive region match"},
		{"pt-BR-x", []string{"pt-BR"}, true, "recorded extends requested region"},
		{"pt", []string{"pt-BR"}, false, "bare pt is narrower-excluded by pt-BR request"},
		{"pt-PT", []string{"pt-BR"}, false, "pt-PT excluded by pt-BR request"},
		// Script narrowing: zh-Hans vs zh-Hant vs bare zh.
		{"zh-Hans", []string{"zh-Hans"}, true, "exact script match"},
		{"zh-Hans-CN", []string{"zh-Hans"}, true, "recorded extends requested script"},
		{"zh-Hant", []string{"zh-Hans"}, false, "opposite script excluded"},
		{"zh", []string{"zh-Hans"}, false, "bare zh excluded by zh-Hans request"},
		// A bare-primary request still matches all region/script extensions.
		{"pt-BR", []string{"pt"}, true, "bare request matches region extension"},
		{"pt-PT", []string{"pt"}, true, "bare request matches other region extension"},
		{"pt", []string{"pt"}, true, "bare request matches bare recorded"},
		// Prefix must land on a subtag boundary.
		{"ptx", []string{"pt"}, false, "pt must not match ptx (boundary)"},
		// Logical OR across the requested set.
		{"pt-BR", []string{"es", "pt-BR"}, true, "OR across set"},
		// Unknown recorded never matches.
		{"", []string{"pt-BR"}, false, "unknown recorded never matches"},
	}
	for _, tc := range cases {
		got := model.LanguageMatchesAnyMode(tc.recorded, tc.requested, model.LanguageMatchStrict)
		if got != tc.want {
			t.Errorf("strict LanguageMatchesAnyMode(%q, %v) = %v, want %v (%s)", tc.recorded, tc.requested, got, tc.want, tc.why)
		}
	}
}

// TestFilterMatch_LanguageMatchStrict confirms the mode threads through
// model.Filter.Match (the pushdown/HNSW/disk path): strict narrows pt-BR away
// from pt-PT, while the same filter under the default mode keeps it.
func TestFilterMatch_LanguageMatchStrict(t *testing.T) {
	ptBR := model.IndexPayload{ChunkID: 1, RelPath: "br.txt", DocType: "text", Language: "pt-BR"}
	ptPT := model.IndexPayload{ChunkID: 2, RelPath: "pt.txt", DocType: "text", Language: "pt-PT"}

	strict := model.Filter{Languages: []string{"pt-BR"}, LanguageMatch: model.LanguageMatchStrict}
	if !strict.Match(ptBR) {
		t.Error("strict [pt-BR] must match a pt-BR payload")
	}
	if strict.Match(ptPT) {
		t.Error("strict [pt-BR] must NOT match a pt-PT payload (region narrowing)")
	}

	// Default mode (empty LanguageMatch) keeps both under primary-subtag matching.
	def := model.Filter{Languages: []string{"pt-BR"}}
	if !def.Match(ptBR) || !def.Match(ptPT) {
		t.Error("default primary mode must match both pt-BR and pt-PT for a [pt-BR] filter")
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
