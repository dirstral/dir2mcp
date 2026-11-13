package providerhttp

import (
	"encoding/json"
)

// WellFormedEmptySTT reports whether raw is a well-formed speech-to-text
// response, the shape a no-speech result needs (SPEC 0.93.0, §8.2). The caller
// has already found that the response holds no text; this answers whether that
// emptiness is a successful decode or a malformed body.
//
// A response is well-formed when all of these are true:
//   - it parses as a JSON object;
//   - it has at least one of textFields or segmentsField;
//   - every text field it has is a string;
//   - its segments field, when present, is an array of objects, and the text of
//     each segment, when present, is a string.
//
// An OpenAI-compatible server answers music or silence with
// `{"text":"","segments":[]}`: well-formed, and a no-speech result. A body that
// names neither field, or a field of the wrong type (a null text, a segments
// object), is malformed and stays a provider error.
func WellFormedEmptySTT(raw []byte, textFields []string, segmentsField string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	present := false
	for _, f := range textFields {
		v, ok := obj[f]
		if !ok {
			continue
		}
		present = true
		var s string
		if !isJSONString(v, &s) {
			return false
		}
	}
	if v, ok := obj[segmentsField]; ok {
		present = true
		if !segmentsWellFormed(v) {
			return false
		}
	}
	return present
}

// segmentsWellFormed reports whether v is an array of objects whose text,
// when present, is a string.
func segmentsWellFormed(v json.RawMessage) bool {
	var segs []map[string]json.RawMessage
	if err := json.Unmarshal(v, &segs); err != nil || segs == nil {
		return false
	}
	for _, seg := range segs {
		if seg == nil {
			return false
		}
		if t, ok := seg["text"]; ok {
			var s string
			if !isJSONString(t, &s) {
				return false
			}
		}
	}
	return true
}

// isJSONString reports whether v is a JSON string. A JSON null is not one:
// encoding/json would leave a Go string untouched for it and report no error.
func isJSONString(v json.RawMessage, out *string) bool {
	if len(v) == 0 || v[0] != '"' {
		return false
	}
	return json.Unmarshal(v, out) == nil
}
