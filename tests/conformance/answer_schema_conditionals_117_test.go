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

// TestAnswerSurfaces_TheConditionalsBindTheRightRelationships_117 stops the
// guard above from passing on riders that merely MENTION the right words.
//
// Equality alone is satisfied by two schemas that drifted identically, and a
// substring check is satisfied by any structure containing the strings. What
// 9.4.5 actually requires is two relationships, so both are decoded and
// asserted as relationships.
func TestAnswerSurfaces_TheConditionalsBindTheRightRelationships_117(t *testing.T) {
	t.Parallel()
	for tool, file := range answerSurfaces {
		tool, file := tool, file
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			riders := decodeRiders(t, servedOutputSchema(t, tool)["allOf"], tool)

			// retrieval_only implies a reason, and implies faithfulness unchecked.
			forward := riderWhoseIfConstrains(riders, "answer_source", "retrieval_only")
			if forward == nil {
				t.Fatalf("served %s (%s) states no rider conditioned on "+
					"answer_source=retrieval_only", tool, file)
			}
			if !riderThenRequires(forward, "answer_source_reason") {
				t.Errorf("served %s: retrieval_only does not require answer_source_reason: %#v",
					tool, forward)
			}
			if got := riderThenConst(forward, "faithfulness"); got != "unchecked" {
				t.Errorf("served %s: retrieval_only constrains faithfulness to %q, want \"unchecked\"",
					tool, got)
			}

			// A reason implies retrieval_only.
			reverse := riderWhoseIfRequires(riders, "answer_source_reason")
			if reverse == nil {
				t.Fatalf("served %s (%s) states no rider conditioned on the presence "+
					"of answer_source_reason", tool, file)
			}
			if got := riderThenConst(reverse, "answer_source"); got != "retrieval_only" {
				t.Errorf("served %s: a reason constrains answer_source to %q, want \"retrieval_only\"",
					tool, got)
			}
		})
	}
}

// decodeRiders turns the schema's allOf into a list of objects.
func decodeRiders(t *testing.T, raw interface{}, label string) []map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal %s allOf: %v", label, err)
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatalf("%s allOf is not a list of objects: %v (%s)", label, err, encoded)
	}
	if len(list) == 0 {
		t.Fatalf("%s declares an empty allOf; nothing to assert", label)
	}
	return list
}

// subObject reads a nested object, returning nil when any step is absent.
func subObject(node map[string]interface{}, path ...string) map[string]interface{} {
	current := node
	for _, key := range path {
		if current == nil {
			return nil
		}
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func stringList(node map[string]interface{}, key string) []string {
	listed, ok := node[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(listed))
	for _, entry := range listed {
		if value, ok := entry.(string); ok {
			out = append(out, value)
		}
	}
	return out
}

// riderWhoseIfConstrains finds the rider whose `if` pins field to value.
func riderWhoseIfConstrains(riders []map[string]interface{}, field, value string) map[string]interface{} {
	for _, rider := range riders {
		prop := subObject(rider, "if", "properties", field)
		if prop != nil && prop["const"] == value {
			return rider
		}
	}
	return nil
}

// riderWhoseIfRequires finds the rider conditioned on a field's PRESENCE.
func riderWhoseIfRequires(riders []map[string]interface{}, field string) map[string]interface{} {
	for _, rider := range riders {
		cond := subObject(rider, "if")
		if cond == nil {
			continue
		}
		// Presence alone: a rider that also pins a const is the other one.
		if subObject(cond, "properties", field) != nil {
			continue
		}
		for _, required := range stringList(cond, "required") {
			if required == field {
				return rider
			}
		}
	}
	return nil
}

func riderThenRequires(rider map[string]interface{}, field string) bool {
	then := subObject(rider, "then")
	if then == nil {
		return false
	}
	for _, required := range stringList(then, "required") {
		if required == field {
			return true
		}
	}
	return false
}

// riderThenConst reads the value `then` pins field to, or "" when unpinned.
func riderThenConst(rider map[string]interface{}, field string) string {
	prop := subObject(rider, "then", "properties", field)
	if prop == nil {
		return ""
	}
	value, _ := prop["const"].(string)
	return value
}
