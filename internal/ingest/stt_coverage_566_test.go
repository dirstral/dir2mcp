package ingest

import (
	"strings"
	"testing"
)

// TestSTTTranscriptMeta_LanguageCovered_566 verifies the honest-coverage flag on
// a machine-transcribed transcript's meta_json (SPEC §8.2.1, #566):
//   - a declared coverage set that excludes the effective language => language_covered:false
//   - a declared set that includes it => the field is ABSENT (covered, no false)
//   - no declaration => ABSENT (unknown, never a misleading false)
//
// The transcript language is pinned (transcriptLanguage) so the meta writer takes
// the "configured" branch and never invokes the auto-detector.
func TestSTTTranscriptMeta_LanguageCovered_566(t *testing.T) {
	metaFor := func(lang string, coverage []string) string {
		s := &Service{transcriptLanguage: lang, sttProvider: "whisper", sttModel: "large-v3", sttLanguages: coverage}
		out, err := s.sttTranscriptMetaJSON(nil, "some transcript text", false)
		if err != nil {
			t.Fatalf("sttTranscriptMetaJSON: %v", err)
		}
		return out
	}

	// Declared, uncovered (the #566 Kyrgyz-on-a-ru/en model case).
	if m := metaFor("kir", []string{"ru", "en"}); !strings.Contains(m, `"language_covered":false`) {
		t.Fatalf("declared+uncovered must record language_covered:false, got %s", m)
	}
	// Declared and covered => flag absent (never true, never a false).
	if m := metaFor("ru", []string{"ru", "en"}); strings.Contains(m, "language_covered") {
		t.Fatalf("covered language must OMIT language_covered, got %s", m)
	}
	// No declaration => unknown => absent.
	if m := metaFor("kir", nil); strings.Contains(m, "language_covered") {
		t.Fatalf("undeclared coverage must OMIT language_covered, got %s", m)
	}
}
