package conformance

import (
	"encoding/json"
	"reflect"
	"testing"
)

// SPEC 9.4.5 (spec 0.70.0): the two answer-provenance fields are PAIRED, and
// the pairing is stated in the canonical schemas as draft-07 conditionals.
//
// The existing conformance guards compare served and canonical PROPERTY NAMES
// and `required`. That is blind to a conditional, and the blindness is not
// theoretical: dir2mcp#1017 shipped the two properties on the served answer
// schemas and left the riders behind in the canonical contract, and every
// conformance test stayed green. A client validates against what the server
// ADVERTISED, so the canonical carrying a rule the served schema omits helps
// nobody.
//
// This guard is deliberately about the conditionals rather than about
// answer_source specifically. Any future rider added canonically and forgotten
// in the server fails here.

// answerSurfaces maps each served tool to its canonical schema file. Kept
// together because 9.4.5 declares the fields on all three at once, precisely
// to avoid the drift 0.56.0 had to correct for `evidence`.
var answerSurfaces = map[string]string{
	"dir2mcp_ask":                "ask.json",
	"dir2mcp_ask_audio":          "ask_audio.json",
	"dir2mcp_transcribe_and_ask": "transcribe_and_ask.json",
}

// normalizeJSON round-trips a value through encoding/json so a Go-built schema
// and a parsed canonical one compare as the same shapes. Without it []string
// and []interface{} differ under DeepEqual for identical JSON.
func normalizeJSON(t *testing.T, v interface{}, label string) interface{} {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", label, err)
	}
	return out
}

func TestAnswerSurfaces_ServedConditionalsMatchTheCanonical_117(t *testing.T) {
	t.Parallel()
	for tool, file := range answerSurfaces {
		tool, file := tool, file
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			canonical := canonicalToolOutput(t, file)
			wantRaw, ok := canonical["allOf"]
			if !ok {
				t.Fatalf("canonical %s declares no allOf; this guard has nothing to "+
					"compare and would pass vacuously", file)
			}
			want := normalizeJSON(t, wantRaw, "canonical "+file+" allOf")
			if list, ok := want.([]interface{}); !ok || len(list) == 0 {
				t.Fatalf("canonical %s allOf is empty: %#v", file, want)
			}

			served := servedOutputSchema(t, tool)
			gotRaw, ok := served["allOf"]
			if !ok {
				t.Fatalf("served %s outputSchema declares no allOf, but canonical %s "+
					"does: a client validating the advertised schema cannot enforce "+
					"the 9.4.5 pairing (#1017)", tool, file)
			}
			got := normalizeJSON(t, gotRaw, "served "+tool+" allOf")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("served %s allOf = %#v, canonical %s = %#v", tool, got, file, want)
			}
		})
	}
}

// TestAnswerSurfaces_TheConditionalsActuallyBindTheProvenanceFields_117 stops
// the guard above from passing on riders that constrain something else
// entirely. It asserts the canonical conditionals mention both field names, so
// a canonical schema that dropped the pairing fails here rather than silently
// reducing the guard to comparing two empty-ish shapes.
func TestAnswerSurfaces_TheConditionalsActuallyBindTheProvenanceFields_117(t *testing.T) {
	t.Parallel()
	for tool, file := range answerSurfaces {
		tool, file := tool, file
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(servedOutputSchema(t, tool)["allOf"])
			if err != nil {
				t.Fatalf("marshal served %s allOf: %v", tool, err)
			}
			for _, needle := range []string{
				"answer_source", "answer_source_reason", "retrieval_only", "unchecked",
			} {
				if !jsonContains(raw, needle) {
					t.Fatalf("served %s allOf does not bind %q (%s): %s",
						tool, needle, file, string(raw))
				}
			}
		})
	}
}

func jsonContains(haystack []byte, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		bytesIndex(haystack, []byte(needle)) >= 0
}

func bytesIndex(haystack, needle []byte) int {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := range needle {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
