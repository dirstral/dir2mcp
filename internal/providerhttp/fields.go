package providerhttp

import (
	"bytes"
	"encoding/json"
)

// WellFormedEmptySTT reports whether raw is a well-formed speech-to-text
// response, the shape a no-speech result needs (SPEC 0.97.0, §8.2). The caller
// has already found that the response holds no text; this answers whether that
// emptiness is a successful decode or a malformed body.
//
// A response is well-formed when all of these are true:
//   - it parses as a JSON object;
//   - it has at least one of textFields or segmentsField;
//   - every text field it has is a string;
//   - its segments field, when present, is an array of objects, and the text of
//     each segment, when present, is a string;
//   - no segment has a start or an end but no text: such a segment reports a
//     timed range of speech that the text does not hold. A segment with an
//     empty text ("text":"") is well-formed;
//   - it carries no timed words: no top-level words array and no words array
//     inside a segment with at least one entry. The caller found no text, so
//     such an array reports speech that the text does not hold. An empty
//     words array is well-formed;
//   - a words field (top level or inside a segment) that is present is an
//     array. An object, a string, a number or a boolean is malformed. A JSON
//     null counts as an absent field: servers that build the response from a
//     typed model with an optional word list send "words": null when word
//     timing was not requested, so a null there says "no words", not a
//     wrong type.
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
	if !wordsEmptyOrAbsent(obj["words"]) {
		return false
	}
	return present
}

// segmentsWellFormed reports whether v is an array of objects whose text,
// when present, is a string, and in which no segment has a start or an end
// with no text key.
func segmentsWellFormed(v json.RawMessage) bool {
	var segs []map[string]json.RawMessage
	if err := json.Unmarshal(v, &segs); err != nil || segs == nil {
		return false
	}
	for _, seg := range segs {
		if seg == nil {
			return false
		}
		if !wordsEmptyOrAbsent(seg["words"]) {
			return false
		}
		t, ok := seg["text"]
		if !ok {
			if segmentIsTimed(seg) {
				return false
			}
			continue
		}
		var s string
		if !isJSONString(t, &s) {
			return false
		}
	}
	return true
}

// wordsEmptyOrAbsent reports whether a words field holds no timed word and is
// well-formed: the field is absent, a JSON null, or an empty array. An array
// with at least one entry is false (timed words with no text), and so is a
// value of another type (an object, a string, a number, a boolean).
func wordsEmptyOrAbsent(v json.RawMessage) bool {
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	if trimmed[0] != '[' {
		return false
	}
	var entries []json.RawMessage
	return json.Unmarshal(trimmed, &entries) == nil && len(entries) == 0
}

// segmentTimeKeys are the keys under which an adapter reads the start or the
// end of a segment: seconds (start, end) and the millisecond form that the
// ElevenLabs parser also reads (start_ms, end_ms).
var segmentTimeKeys = []string{"start", "end", "start_ms", "end_ms"}

// segmentIsTimed reports whether a segment names a start or an end.
func segmentIsTimed(seg map[string]json.RawMessage) bool {
	for _, k := range segmentTimeKeys {
		if _, ok := seg[k]; ok {
			return true
		}
	}
	return false
}

// isJSONString reports whether v is a JSON string. A JSON null is not one:
// encoding/json would leave a Go string untouched for it and report no error.
func isJSONString(v json.RawMessage, out *string) bool {
	if len(v) == 0 || v[0] != '"' {
		return false
	}
	return json.Unmarshal(v, out) == nil
}
