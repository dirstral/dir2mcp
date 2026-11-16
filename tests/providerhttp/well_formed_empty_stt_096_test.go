package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/providerhttp"
)

// TestWellFormedEmptySTT pins the SPEC 0.96.0 §8.2 shape rule that tells a
// no-speech result from a malformed response: it parses, it has a text or a
// segments field, and every field it has is of the right type.
func TestWellFormedEmptySTT(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"text":"","segments":[]}`, true},
		{`{"task":"transcribe","language":"nn","duration":60.0,"text":"","segments":[]}`, true},
		{`{"text":""}`, true},
		{`{"segments":[]}`, true},
		{`{"segments":[{"start":0,"end":1,"text":" "}]}`, true},
		// A segment with an empty text is well-formed. A segment with a start
		// or an end but no text key is malformed: it reports a timed range of
		// speech that the text does not hold.
		{`{"segments":[{"start":0,"end":1,"text":""}]}`, true},
		{`{"text":"","segments":[{"start":0,"end":1,"text":""}]}`, true},
		{`{"segments":[{}]}`, true},
		// No time field and no text: well-formed, so a no-speech result.
		{`{"segments":[{"id":0}]}`, true},
		{`{"segments":[{"start":0,"end":1}]}`, false},
		{`{"segments":[{"start":0}]}`, false},
		{`{"segments":[{"end":1}]}`, false},
		// The millisecond keys that the ElevenLabs parser reads.
		{`{"segments":[{"start_ms":1000}]}`, false},
		{`{"segments":[{"end_ms":2000}]}`, false},
		{`{"segments":[{"start_ms":1000,"text":""}]}`, true},
		{`{"text":"","segments":[{"start":0,"end":1,"text":""},{"start":1,"end":2}]}`, false},
		{`{}`, false},
		{`null`, false},
		{`[]`, false},
		{`not json`, false},
		{`{"text":null}`, false},
		{`{"text":null,"segments":[]}`, false},
		{`{"text":5}`, false},
		{`{"text":"","segments":null}`, false},
		{`{"text":"","segments":{}}`, false},
		{`{"segments":["x"]}`, false},
		{`{"segments":[null]}`, false},
		{`{"segments":[{"text":7}]}`, false},
		{`{"language":"en"}`, false},
	} {
		if got := providerhttp.WellFormedEmptySTT([]byte(tc.body), []string{"text"}, "segments"); got != tc.want {
			t.Errorf("WellFormedEmptySTT(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
