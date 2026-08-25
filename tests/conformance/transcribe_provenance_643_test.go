package conformance

// The transcribe tools used to return provenance under names the canonical
// schemas never declared (#643): dir2mcp_transcribe said `provider` where
// transcribe.json requires `stt_provider`, and dir2mcp_transcribe_and_ask said
// `transcript_provider` where transcribe_and_ask.json requires `stt_provider`.
// Both canonical output objects close themselves with
// `additionalProperties: false`, so every successful runtime payload failed
// canonical validation, and clients generated from the canonical schemas could
// not deserialize a successful response.
//
// These tests pin the canonical names on the schema inside the dirstral-spec
// submodule rather than on a copy, so a spec-side change reaches them through
// the submodule pin (the tests/conformance/clip_size_contract_878_test.go
// idiom), and hold the SERVED outputSchema to the canonical property and
// required sets so the runtime cannot drift back.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// canonicalToolOutput returns the top-level "output" subschema of the named
// canonical tool schema inside the dirstral-spec submodule as a generic tree.
func canonicalToolOutput(t *testing.T, schemaFile string) map[string]interface{} {
	t.Helper()
	path := filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", schemaFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read canonical %s (run: git submodule update --init): %v", schemaFile, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical %s: %v", schemaFile, err)
	}
	part, ok := doc["output"]
	if !ok {
		t.Fatalf("canonical %s declares no output subschema", schemaFile)
	}
	var tree map[string]interface{}
	if err := json.Unmarshal(part, &tree); err != nil {
		t.Fatalf("decode canonical %s output subschema: %v", schemaFile, err)
	}
	return tree
}

// servedOutputSchema returns the served outputSchema for the named tool.
func servedOutputSchema(t *testing.T, tool string) map[string]interface{} {
	t.Helper()
	schemas := toolsListSchemas(t)
	raw, ok := schemas[tool]
	if !ok {
		t.Fatalf("tools/list advertises no %s", tool)
	}
	served, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("served %s outputSchema is not an object: %#v", tool, raw)
	}
	return served
}

// assertContains fails unless every name in want is present in got.
func assertContains(t *testing.T, got []string, want []string, label string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, name := range got {
		set[name] = true
	}
	for _, name := range want {
		if !set[name] {
			t.Fatalf("%s = %v, missing %q", label, got, name)
		}
	}
}

// assertAbsent fails when any of the retired names is still present.
func assertAbsent(t *testing.T, got []string, retired []string, label string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, name := range got {
		set[name] = true
	}
	for _, name := range retired {
		if set[name] {
			t.Fatalf("%s = %v, still carries the retired name %q (#643)", label, got, name)
		}
	}
}

// TestTranscribe_CanonicalOutputNamesSTTProvider_643 pins the canonical half of
// the contract: transcribe.json declares and requires `stt_provider`, stays
// closed, and never re-grows the retired `provider` name.
func TestTranscribe_CanonicalOutputNamesSTTProvider_643(t *testing.T) {
	t.Parallel()
	output := canonicalToolOutput(t, "transcribe.json")
	assertClosedObject(t, output, "canonical transcribe output")

	props := schemaPropertyNames(t, output, "canonical transcribe output")
	assertContains(t, props, []string{"stt_provider", "model"}, "canonical transcribe output properties")
	assertAbsent(t, props, []string{"provider", "transcript_provider"}, "canonical transcribe output properties")

	required := schemaRequired(t, output, "canonical transcribe output")
	assertContains(t, required, []string{"stt_provider"}, "canonical transcribe output required")
	assertAbsent(t, required, []string{"provider"}, "canonical transcribe output required")
}

// TestTranscribeAndAsk_CanonicalOutputNamesSTTProvider_643 is the same pin for
// transcribe_and_ask.json: `stt_provider` plus `transcript_model` are declared
// and required; `transcript_provider` never comes back.
func TestTranscribeAndAsk_CanonicalOutputNamesSTTProvider_643(t *testing.T) {
	t.Parallel()
	output := canonicalToolOutput(t, "transcribe_and_ask.json")
	assertClosedObject(t, output, "canonical transcribe_and_ask output")

	props := schemaPropertyNames(t, output, "canonical transcribe_and_ask output")
	assertContains(t, props, []string{"stt_provider", "transcript_model"}, "canonical transcribe_and_ask output properties")
	assertAbsent(t, props, []string{"provider", "transcript_provider"}, "canonical transcribe_and_ask output properties")

	required := schemaRequired(t, output, "canonical transcribe_and_ask output")
	assertContains(t, required, []string{"stt_provider", "transcript_model"}, "canonical transcribe_and_ask output required")
	assertAbsent(t, required, []string{"transcript_provider"}, "canonical transcribe_and_ask output required")
}

// TestTranscribe_ServedOutputMatchesTheCanonicalContract_643 keeps the code
// from drifting on its own: the served dir2mcp_transcribe outputSchema must
// declare and require exactly the canonical property set. Any difference is
// drift; the canonical object closes itself with additionalProperties:false,
// so a canonically validating client rejects the whole call on an undeclared
// field (#850 class).
func TestTranscribe_ServedOutputMatchesTheCanonicalContract_643(t *testing.T) {
	t.Parallel()
	served := servedOutputSchema(t, "dir2mcp_transcribe")
	assertClosedObject(t, served, "served transcribe outputSchema")

	canonical := canonicalToolOutput(t, "transcribe.json")
	wantProps := schemaPropertyNames(t, canonical, "canonical transcribe output")
	gotProps := schemaPropertyNames(t, served, "served transcribe outputSchema")
	if !reflect.DeepEqual(gotProps, wantProps) {
		t.Fatalf("served transcribe output properties = %v, canonical = %v (#643)", gotProps, wantProps)
	}

	wantRequired := schemaRequired(t, canonical, "canonical transcribe output")
	gotRequired := schemaRequired(t, served, "served transcribe outputSchema")
	if !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Fatalf("served transcribe output required = %v, canonical = %v (#643)", gotRequired, wantRequired)
	}
}

// TestTranscribeAndAsk_ServedOutputMatchesTheCanonicalContract_643 is the same
// guard for dir2mcp_transcribe_and_ask, with one pinned exception: the served
// schema inherits ask's optional `evidence` verdict (#896), which the canonical
// transcribe_and_ask.json does not declare yet. Declaring it is a spec-side
// change, so the exception is tolerated only while the canonical schema lacks
// the name; the moment the spec declares `evidence`, the filter below stops
// removing it and the comparison tightens to exact equality on its own. Any
// OTHER difference is drift today.
func TestTranscribeAndAsk_ServedOutputMatchesTheCanonicalContract_643(t *testing.T) {
	t.Parallel()
	served := servedOutputSchema(t, "dir2mcp_transcribe_and_ask")
	assertClosedObject(t, served, "served transcribe_and_ask outputSchema")

	canonical := canonicalToolOutput(t, "transcribe_and_ask.json")
	wantProps := schemaPropertyNames(t, canonical, "canonical transcribe_and_ask output")
	gotProps := schemaPropertyNames(t, served, "served transcribe_and_ask outputSchema")
	gotProps = withoutKnownExtras(gotProps, wantProps, []string{"evidence"})
	if !reflect.DeepEqual(gotProps, wantProps) {
		t.Fatalf("served transcribe_and_ask output properties = %v, canonical = %v (#643)", gotProps, wantProps)
	}

	wantRequired := schemaRequired(t, canonical, "canonical transcribe_and_ask output")
	gotRequired := schemaRequired(t, served, "served transcribe_and_ask outputSchema")
	if !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Fatalf("served transcribe_and_ask output required = %v, canonical = %v (#643)", gotRequired, wantRequired)
	}
}

// withoutKnownExtras drops the named extras from got, but only while the
// canonical set does not declare them, so a later spec-side declaration
// tightens the guard automatically instead of leaving a stale allowlist.
func withoutKnownExtras(got, want, extras []string) []string {
	wantSet := make(map[string]bool, len(want))
	for _, name := range want {
		wantSet[name] = true
	}
	extraSet := make(map[string]bool, len(extras))
	for _, name := range extras {
		if !wantSet[name] {
			extraSet[name] = true
		}
	}
	out := make([]string, 0, len(got))
	for _, name := range got {
		if !extraSet[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
