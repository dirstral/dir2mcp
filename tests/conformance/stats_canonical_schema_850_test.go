package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Whole-payload conformance for dir2mcp_stats (issue #850).
//
// SPEC §1.3 treats "the schema the server advertises" and "the schema for the
// payload's declared format_version" as ONE schema, and the canonical stats
// output object is closed (additionalProperties: false). So a payload that
// carries a field the canonical schema does not declare is invalid for every
// client that validates against the canonical copy, on every single call.
//
// dir2mcp used to emit a `sessions` object and to mark it required in the served
// schema. The canonical schema declares no such field, so a canonically
// validating client rejected EVERY stats response. #849 could only validate the
// skip_reasons subtree for that reason. These tests validate the WHOLE object,
// which is what makes the drift impossible to reintroduce quietly.

// canonicalStatsSchemaPath is the pinned canonical schema inside the
// dirstral-spec submodule. The suite reads THAT file, not a copy, so a
// spec-side change reaches this test through the submodule pin.
var canonicalStatsSchemaPath = filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "stats.json")

// canonicalStatsOutputRaw returns the raw `definitions.output` subschema of the
// canonical stats.json. That subschema, not the file's root, is the contract for
// a dir2mcp_stats structuredContent payload.
func canonicalStatsOutputRaw(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(canonicalStatsSchemaPath)
	if err != nil {
		t.Fatalf("read canonical stats.json (run: git submodule update --init): %v", err)
	}
	var doc struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical stats.json: %v", err)
	}
	output, ok := doc.Definitions["output"]
	if !ok {
		t.Fatal("canonical stats.json declares no definitions.output")
	}
	return output
}

// canonicalStatsValidator compiles the canonical output subschema into a
// validator.
func canonicalStatsValidator(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(canonicalStatsOutputRaw(t), &schema); err != nil {
		t.Fatalf("decode canonical stats output subschema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve canonical stats output subschema: %v", err)
	}
	return resolved
}

// canonicalStatsTree returns the canonical output subschema as a generic tree,
// for the property-set and required-list comparison.
func canonicalStatsTree(t *testing.T) map[string]interface{} {
	t.Helper()
	var tree map[string]interface{}
	if err := json.Unmarshal(canonicalStatsOutputRaw(t), &tree); err != nil {
		t.Fatalf("decode canonical stats output subschema as a tree: %v", err)
	}
	return tree
}

// callStatsStructured calls dir2mcp_stats over the production transport and
// returns the structuredContent a real client validates.
func callStatsStructured(t *testing.T, srv *runningServer, cfg config.Config) map[string]interface{} {
	t.Helper()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	resp := sendRPC(t, mcpURL, sid, statsCallBody(2), nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("stats: decode response: %v body=%s", err, body)
	}
	if envelope.Result.IsError {
		t.Fatalf("stats: isError=true body=%s", body)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("stats: missing structuredContent body=%s", body)
	}
	return envelope.Result.StructuredContent
}

// assertCanonicalStats validates a whole stats payload against the canonical
// schema and reports the offending payload on failure.
func assertCanonicalStats(t *testing.T, structured map[string]interface{}) {
	t.Helper()
	if err := canonicalStatsValidator(t).Validate(structured); err != nil {
		pretty, _ := json.MarshalIndent(structured, "", "  ")
		t.Fatalf("dir2mcp_stats payload is invalid against the canonical stats.json: %v\npayload:\n%s", err, pretty)
	}
}

// TestStats_PayloadValidatesAgainstCanonicalSchema pins the whole payload of a
// plain server against the canonical schema.
//
// It fails on main with `unexpected additional properties ["sessions"]`, which
// is exactly what a strict client saw on every call.
func TestStats_PayloadValidatesAgainstCanonicalSchema(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	srv := newServer(t, cfg)
	defer srv.Close()

	assertCanonicalStats(t, callStatsStructured(t, srv, cfg))
}

// statsStoreWithGaps seeds a store whose corpus exercises the optional halves of
// the §15.6 output: skipped documents (skip_reasons) and a failed one
// (recent_failures). Without them a whole-payload check would never reach those
// subtrees.
func statsStoreWithGaps(t *testing.T) (model.Store, string) {
	t.Helper()
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seeded := []model.Document{
		{RelPath: "a.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat},
		{RelPath: "b.rtf", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat},
		{RelPath: "c.zip", DocType: "archive", Status: "skipped", SkipReason: model.SkipReasonArchive},
		{RelPath: "d.md", DocType: "md", Status: "ok"},
		{RelPath: "e.pdf", DocType: "pdf", Status: "error", ErrorMessage: "extract failed"},
	}
	for _, doc := range seeded {
		if err := st.UpsertDocument(context.Background(), doc); err != nil {
			t.Fatalf("seed %s: %v", doc.RelPath, err)
		}
	}
	return st, tmp
}

// TestStats_PopulatedPayloadValidatesAgainstCanonicalSchema validates the whole
// payload of a corpus that actually has coverage gaps, so skip_reasons and
// recent_failures are present and get validated too. This is the check #849
// could not write: it had to look at the skip_reasons subtree alone, because a
// whole-payload validation failed on `sessions` no matter what the subtree said.
func TestStats_PopulatedPayloadValidatesAgainstCanonicalSchema(t *testing.T) {
	t.Parallel()
	st, tmp := statsStoreWithGaps(t)

	cfg := defaultConfig()
	cfg.StateDir = tmp
	retriever := retrieval.NewService(st, nil, nil, nil)
	srv := newServerWithRetriever(t, cfg, retriever, mcp.WithStore(st))
	defer srv.Close()

	structured := callStatsStructured(t, srv, cfg)

	// Guard against a vacuous pass: the optional subtrees must really be there.
	for _, field := range []string{"skip_reasons", "recent_failures"} {
		if _, present := structured[field]; !present {
			t.Fatalf("seeded corpus produced no %s, so the whole-payload check would not cover it: %v", field, sortedKeys(structured))
		}
	}
	assertCanonicalStats(t, structured)
}

// sortedKeys returns the sorted key set of a decoded JSON object.
func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// schemaPropertyNames reads a schema node's `properties` key set.
func schemaPropertyNames(t *testing.T, schema map[string]interface{}, label string) []string {
	t.Helper()
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s schema declares no properties object: %#v", label, schema["properties"])
	}
	return sortedKeys(properties)
}

// schemaRequired reads a schema node's `required` list as sorted strings.
func schemaRequired(t *testing.T, schema map[string]interface{}, label string) []string {
	t.Helper()
	raw, ok := schema["required"].([]interface{})
	if !ok {
		t.Fatalf("%s schema declares no required array: %#v", label, schema["required"])
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("%s schema has a non-string entry in required: %#v", label, item)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestStats_AdvertisedSchemaMatchesCanonical pins the other direction of the
// same contract. The payload check above only sees what this deployment happens
// to emit. This one reads the schema the server publishes through tools/list and
// requires the same top-level property set and the same required list as the
// canonical file.
//
// It matters on its own: `required` in a served schema is a promise about the
// ecosystem shape, not about this deployment's extras. A served schema that
// required `sessions` made a client refuse a conforming response from another
// implementation that correctly omits it.
//
// It fails on main, where `sessions` is both advertised and required.
//
// It compares the top level only. The nested shape stays with the payload
// checks above, which validate real payloads against the whole canonical schema,
// and with the deliberate served/canonical divergence #849 documented for the
// skip_reasons `reason` enum.
func TestStats_AdvertisedSchemaMatchesCanonical(t *testing.T) {
	t.Parallel()
	served, ok := toolsListSchemas(t)[protocol.ToolNameStats].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/list advertises no outputSchema for %s", protocol.ToolNameStats)
	}
	canonical := canonicalStatsTree(t)

	assertSameStrings(t, "stats.json", "top-level property",
		schemaPropertyNames(t, canonical, "canonical"),
		schemaPropertyNames(t, served, "served"))
	assertSameStrings(t, "stats.json", "required field",
		schemaRequired(t, canonical, "canonical"),
		schemaRequired(t, served, "served"))
}

// assertSameStrings reports each name present on one side only. It names the
// direction, because the two directions are different defects: served-only means
// the server publishes a field outside the contract, canonical-only means it
// fails to publish a field the contract defines.
//
// `subject` names the canonical file under comparison, so a second caller (the
// Span check in span_attribution_856_test.go) does not report a Span drift as a
// stats drift.
func assertSameStrings(t *testing.T, subject, label string, canonical, served []string) {
	t.Helper()
	inCanonical := make(map[string]bool, len(canonical))
	for _, name := range canonical {
		inCanonical[name] = true
	}
	inServed := make(map[string]bool, len(served))
	for _, name := range served {
		inServed[name] = true
	}
	for _, name := range served {
		if !inCanonical[name] {
			t.Errorf("the served schema declares %s %q, which the canonical %s does not: served=%v canonical=%v", label, name, subject, served, canonical)
		}
	}
	for _, name := range canonical {
		if !inServed[name] {
			t.Errorf("the canonical %s declares %s %q, which the served schema does not: served=%v canonical=%v", subject, label, name, served, canonical)
		}
	}
}
