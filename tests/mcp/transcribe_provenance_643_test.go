package tests

// Canonical payload validation for the transcribe tools (#643): the runtime
// used to return provenance under names the canonical schemas never declared
// (`provider`, `transcript_provider`), and the canonical output objects close
// themselves with `additionalProperties: false`, so every successful payload
// failed canonical validation. assertCanonicalToolPayload holds a real wire
// payload to the canonical property and required sets, read from the pinned
// dirstral-spec submodule when the checkout is present (the
// stats_skip_reasons_646_test.go idiom), so a spec-side rename cannot pass
// unnoticed. A shallow clone without submodules logs and skips only this
// validation; the calling test's own assertions still run.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func assertCanonicalToolPayload(t *testing.T, schemaFile string, structured map[string]interface{}) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", schemaFile))
	if err != nil {
		t.Logf("canonical %s unavailable (%v); skipping canonical payload validation", schemaFile, err)
		return
	}
	var doc struct {
		Output struct {
			AdditionalProperties *bool                      `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical %s: %v", schemaFile, err)
	}
	if len(doc.Output.Properties) == 0 {
		t.Fatalf("canonical %s declares no output properties", schemaFile)
	}
	if doc.Output.AdditionalProperties == nil || *doc.Output.AdditionalProperties {
		t.Fatalf("canonical %s output is not closed; this validation assumes additionalProperties:false", schemaFile)
	}
	for key := range structured {
		if _, ok := doc.Output.Properties[key]; !ok {
			t.Fatalf("payload field %q is undeclared in the closed canonical %s output; a canonically validating client rejects the whole response (#643)", key, schemaFile)
		}
	}
	for _, key := range doc.Output.Required {
		if _, ok := structured[key]; !ok {
			t.Fatalf("payload lacks %q, required by the canonical %s output (#643); payload: %#v", key, schemaFile, structured)
		}
	}
}
