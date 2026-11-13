package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/providerhttp"
)

// TestWellFormedEmptySTT pins the SPEC 0.93.0 §8.2 shape rule that tells a
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
		{`{"segments":[{"start":0,"end":1}]}`, true},
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
