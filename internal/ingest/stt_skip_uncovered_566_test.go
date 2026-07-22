package ingest

import "testing"

// TestNormalizeOnUncoveredLanguage_566 pins the media.stt.on_uncovered_language
// normalization (SPEC §8.2.1, #566): only "skip" (case-insensitive, trimmed)
// selects the strict skip contract; everything else — including "warn", empty, and
// any unrecognized value — is the fail-open "warn" default, so a Service built from
// an unvalidated config always degrades safely (transcribe anyway).
func TestNormalizeOnUncoveredLanguage_566(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"skip", onUncoveredLanguageSkip},
		{"SKIP", onUncoveredLanguageSkip},
		{"  skip  ", onUncoveredLanguageSkip},
		{"warn", "warn"},
		{"", "warn"},
		{"nonsense", "warn"},
	}
	for _, c := range cases {
		if got := normalizeOnUncoveredLanguage(c.in); got != c.want {
			t.Errorf("normalizeOnUncoveredLanguage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSTTLanguageUncovered_566 pins the honest-coverage floor decision used by the
// skip branch (SPEC §8.2.1, #566). It keys ONLY off the pinned source language and
// the resolved profile's declared coverage — never per-item auto-detection — so it
// trips exactly when a non-empty declared set excludes the pin.
func TestSTTLanguageUncovered_566(t *testing.T) {
	cases := []struct {
		name     string
		pin      string
		coverage []string
		want     bool
	}{
		{"declared+uncovered trips", "kir", []string{"ru", "en"}, true},
		{"declared+covered does not trip", "ru", []string{"ru", "en"}, false},
		{"undeclared (nil) never trips", "kir", nil, false},
		{"empty declared set never trips", "kir", []string{}, false},
		{"no pin never trips", "", []string{"ru", "en"}, false},
		{"blank pin never trips", "   ", []string{"ru", "en"}, false},
	}
	for _, c := range cases {
		s := &Service{transcriptLanguage: c.pin, sttLanguages: c.coverage}
		if got := s.sttLanguageUncovered(); got != c.want {
			t.Errorf("%s: sttLanguageUncovered(pin=%q, coverage=%v) = %v, want %v", c.name, c.pin, c.coverage, got, c.want)
		}
	}
}
