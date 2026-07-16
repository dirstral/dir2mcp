package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestSTTLanguageCoverage_566 pins the declared STT language-coverage helper
// (SPEC §8.2.1, #566): an empty coverage set (or empty language) makes NO
// assertion (declared=false); a non-empty set matches on the BCP-47 primary
// subtag, case-insensitive, so region variants and casing don't defeat it.
func TestSTTLanguageCoverage_566(t *testing.T) {
	cases := []struct {
		name         string
		coverage     []string
		lang         string
		wantDeclared bool
		wantCovered  bool
	}{
		{"no declaration => unknown", nil, "kir", false, false},
		{"empty language => unknown", []string{"ru", "en"}, "", false, false},
		{"covered exact", []string{"ru", "en"}, "ru", true, true},
		{"covered primary-subtag vs region", []string{"ru"}, "ru-RU", true, true},
		{"covered region-declared vs primary", []string{"en-US"}, "en", true, true},
		{"covered case-insensitive", []string{"RU", "EN"}, "ru", true, true},
		{"declared but uncovered (the #566 case)", []string{"ru", "en"}, "kir", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declared, covered := provider.STTLanguageCoverageSet(tc.coverage, tc.lang)
			if declared != tc.wantDeclared || covered != tc.wantCovered {
				t.Fatalf("STTLanguageCoverageSet(%v, %q) = (declared=%v, covered=%v), want (%v, %v)",
					tc.coverage, tc.lang, declared, covered, tc.wantDeclared, tc.wantCovered)
			}
			// The Profile-based wrapper must agree.
			pd, pc := provider.STTLanguageCoverage(provider.Profile{STTLanguages: tc.coverage}, tc.lang)
			if pd != declared || pc != covered {
				t.Fatalf("STTLanguageCoverage wrapper disagrees: (%v,%v) vs (%v,%v)", pd, pc, declared, covered)
			}
		})
	}
}
