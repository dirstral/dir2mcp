package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SPEC §8.2.2 reads the language a Whisper server reports for a decode. The
// OpenAI API and some builds report Whisper's English language NAME
// ("russian") where faster-whisper reports the ISO code ("ru"). A name that
// reached the per-window router unmapped would match no route and sit outside
// every declared coverage, so a covered Russian window would be refused as
// uncovered. The client maps names to codes, passes codes through, and reports
// anything else as unknown so the text detector runs instead.
func TestTranscribeStructured_ReportedLanguageIsNormalized(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported string
		want     string
	}{
		{name: "an ISO code passes through", reported: "ru", want: "ru"},
		{name: "a code with a region keeps its primary subtag", reported: "pt-BR", want: "pt"},
		{name: "an English name maps to its code", reported: "russian", want: "ru"},
		{name: "a name is matched without case", reported: "Ukrainian", want: "uk"},
		{name: "a whisper alias maps to its code", reported: "burmese", want: "my"},
		{name: "a three-letter code passes through", reported: "yue", want: "yue"},
		{name: "an unknown word is unknown, not a language", reported: "klingon", want: ""},
		{name: "nothing reported is unknown", reported: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"text":"hello there","language":%q,"language_probability":0.93,"segments":[{"start":0,"end":1.5,"text":"hello there"}]}`, tc.reported))
			}))
			defer srv.Close()

			c := newClient(srv.URL, "")
			res, err := c.TranscribeStructured(context.Background(), "a.wav", []byte("x"))
			if err != nil {
				t.Fatalf("TranscribeStructured: %v", err)
			}
			if res.Language != tc.want {
				t.Errorf("reported %q: Language = %q, want %q", tc.reported, res.Language, tc.want)
			}
			if tc.want != "" && res.LanguageConfidence != 0.93 {
				t.Errorf("reported %q: LanguageConfidence = %v, want 0.93", tc.reported, res.LanguageConfidence)
			}
			if tc.want == "" && res.LanguageConfidence != 0 {
				t.Errorf("an unknown language must carry no confidence, got %v", res.LanguageConfidence)
			}
		})
	}
}
