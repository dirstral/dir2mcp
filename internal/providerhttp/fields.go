package providerhttp

import (
	"bytes"
	"encoding/json"
)

// HasAnyJSONField reports whether raw is a JSON object that carries at least one
// of the named top-level fields with a non-null value.
//
// The speech-to-text adapters use it to tell a well-formed response that found
// no speech from a malformed one (SPEC 0.93.0). An OpenAI-compatible server
// answers music or silence with `{"text":"","segments":[]}`: the fields are
// present and empty, which is a successful decode with nothing said. A body
// that names neither field (an error envelope with a 200 status, `null`, an
// unrelated object) is not a transcription response at all, and stays an error.
func HasAnyJSONField(raw []byte, fields ...string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	for _, f := range fields {
		if v, ok := obj[f]; ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return true
		}
	}
	return false
}
