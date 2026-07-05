package ingest

import "testing"

// TestSetTranslateTranscriber_FiltersToEnglish pins that the whisper translate
// setter drops non-English target tags: whisper's translate task only ever emits
// English, so a non-English target would otherwise persist English output under
// the wrong language tag (e.g. transcript:fr). English variants (en, en-US) are
// kept; blanks and non-English tags are dropped.
func TestSetTranslateTranscriber_FiltersToEnglish(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"drops non-english", []string{"fr", "en", "de"}, []string{"en"}},
		{"keeps english variants", []string{"en-US", "EN"}, []string{"en-us", "en"}},
		{"drops blanks and all-non-english", []string{"", "  ", "es"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{}
			s.SetTranslateTranscriber(nil, "whisper", "large-v3", tc.in)
			got := s.translateTargetLangs
			if len(got) != len(tc.want) {
				t.Fatalf("target langs = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("target langs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
