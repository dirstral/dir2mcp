package conformance

// dir2mcp_open_media_clip cuts at the SOURCE bitrate, so an 8 second clip of a
// 20 Mbit/s recording is about 22 MB, and about 30 MB base64 on the wire (#878).
// A pilot viewer read that latency as a broken play button.
//
// The fix a caller needs has two halves: a way to ASK for a smaller clip, and a
// response that SAYS which rendition it got. Both are tool-contract changes, and
// both are blocked on dirstral-spec today. The canonical `open_media_clip.json`
// closes the input object AND the output object with
// `additionalProperties: false` and enumerates every property, so:
//
//   - there is no room for a `max_bytes` / `quality` / `rendition` argument, and
//     a strict client could not send one even if the server honored it (#645);
//   - there is no room for a field that tells a preview from the source, so a
//     server that quietly served a smaller clip would be UNDETECTABLE. A caller
//     could not tell a preview from the original, which is worse than the size.
//
// These tests pin that gate on the schema inside the dirstral-spec submodule,
// not on a copy, so a spec-side change reaches them through the submodule pin
// (the tests/conformance/stats_canonical_schema_850_test.go idiom). When the
// spec PR opens the contract, TestOpenMediaClip_CanonicalInputHasNoSizeControl
// MUST fail. Update it then, together with the code that serves the smaller
// clip, and only with the merged spec change behind it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// canonicalOpenMediaClipSchemaPath is the pinned canonical schema inside the
// dirstral-spec submodule.
var canonicalOpenMediaClipSchemaPath = filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "open_media_clip.json")

// canonicalOpenMediaClipSection returns the named top-level subschema ("input"
// or "output") of the canonical open_media_clip.json as a generic tree.
func canonicalOpenMediaClipSection(t *testing.T, section string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(canonicalOpenMediaClipSchemaPath)
	if err != nil {
		t.Fatalf("read canonical open_media_clip.json (run: git submodule update --init): %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical open_media_clip.json: %v", err)
	}
	part, ok := doc[section]
	if !ok {
		t.Fatalf("canonical open_media_clip.json declares no %q subschema", section)
	}
	var tree map[string]interface{}
	if err := json.Unmarshal(part, &tree); err != nil {
		t.Fatalf("decode canonical open_media_clip %s subschema: %v", section, err)
	}
	return tree
}

// assertClosedObject requires additionalProperties:false, which is what makes an
// undeclared field unreachable for a strict client rather than merely unusual.
func assertClosedObject(t *testing.T, schema map[string]interface{}, label string) {
	t.Helper()
	closed, ok := schema["additionalProperties"].(bool)
	if !ok || closed {
		t.Fatalf("%s: additionalProperties = %#v, want false", label, schema["additionalProperties"])
	}
}

// TestOpenMediaClip_CanonicalInputHasNoSizeControl_878 pins the input half of
// the #878 gate: the canonical argument list is closed and holds no way to ask
// for fewer bytes.
func TestOpenMediaClip_CanonicalInputHasNoSizeControl_878(t *testing.T) {
	t.Parallel()
	input := canonicalOpenMediaClipSection(t, "input")
	assertClosedObject(t, input, "canonical open_media_clip input")

	want := []string{"chunk_id", "end_ms", "rel_path", "return", "start_ms"}
	got := schemaPropertyNames(t, input, "canonical open_media_clip input")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical open_media_clip input properties = %v, want %v.\n"+
			"If a size/quality argument was added by a merged dirstral-spec PR, this gate is OPEN: "+
			"implement it in internal/mcp and update this test (#878).", got, want)
	}
}

// TestOpenMediaClip_CanonicalOutputCannotNameWhatItServed_878 pins the output
// half of the gate: no declared field can report a rendition or a quality, so a
// preview would be indistinguishable from the source.
func TestOpenMediaClip_CanonicalOutputCannotNameWhatItServed_878(t *testing.T) {
	t.Parallel()
	output := canonicalOpenMediaClipSection(t, "output")
	assertClosedObject(t, output, "canonical open_media_clip output")

	want := []string{
		"data", "doc_type", "duration_ms", "expires_unix",
		"mime_type", "reference_fallback", "rel_path", "return", "size_bytes",
		"span", "uri",
	}
	// Read the list above as the finding: no member of it can report a served
	// rendition. mime_type is the only field that describes the bytes, and it
	// names the container, not the fidelity. A 360p preview and a 1080p source
	// cut are both "video/mp4", so it cannot carry the distinction.
	//
	// reference_fallback joined the list in dirstral-spec 0.53.0 and does NOT
	// open this gate. It reports the CARRIER, not the fidelity: it says the
	// caller asked for a reference and got inline bytes. A server that quietly
	// served a 360p cut instead of the source would set neither this field nor
	// any other, so a preview is still indistinguishable from the original and
	// #878 stays blocked on the input half of the contract.
	got := schemaPropertyNames(t, output, "canonical open_media_clip output")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical open_media_clip output properties = %v, want %v.\n"+
			"If a field naming the served rendition was added by a merged dirstral-spec PR, this gate is OPEN: "+
			"emit it from internal/mcp and update this test (#878).", got, want)
	}
}

// TestOpenMediaClip_ServedInputMatchesTheCanonicalContract_878 keeps the code
// from opening the gate on its own. A server that quietly accepted an
// undeclared size argument would be unreachable for a schema-driven client and
// would drift from the spec at the same time.
func TestOpenMediaClip_ServedInputMatchesTheCanonicalContract_878(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	schemas := toolInputSchemas(t, mcpURL, sid)
	served, ok := schemas["dir2mcp_open_media_clip"]
	if !ok {
		t.Fatal("tools/list advertises no dir2mcp_open_media_clip")
	}
	assertClosedObject(t, served, "served open_media_clip inputSchema")

	want := schemaPropertyNames(t, canonicalOpenMediaClipSection(t, "input"), "canonical open_media_clip input")
	got := schemaPropertyNames(t, served, "served open_media_clip inputSchema")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("served open_media_clip input properties = %v, canonical = %v", got, want)
	}
}

// TestOpenMediaClip_ServedOutputMatchesTheCanonicalContract_878 is the output
// half of the same guard: the code must not open the #878 gate by declaring a
// field the canonical schema does not have.
//
// It used to carry one known exception, `reference_fallback`: the server
// declared and emitted that field while the canonical output object did not
// declare it and closed itself with additionalProperties:false, so a
// canonically validating client rejected every return=reference response
// (dir2mcp #884, the #850 defect class in a smaller blast radius). dirstral-spec
// 0.53.0 declares the field, so served and canonical agree exactly and the
// exception is gone. There is no allowlist left: any difference here is drift.
func TestOpenMediaClip_ServedOutputMatchesTheCanonicalContract_878(t *testing.T) {
	t.Parallel()
	schemas := toolsListSchemas(t)
	raw, ok := schemas["dir2mcp_open_media_clip"]
	if !ok {
		t.Fatal("tools/list advertises no dir2mcp_open_media_clip")
	}
	served, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("served open_media_clip outputSchema is not an object: %#v", raw)
	}
	assertClosedObject(t, served, "served open_media_clip outputSchema")

	want := schemaPropertyNames(t, canonicalOpenMediaClipSection(t, "output"), "canonical open_media_clip output")
	got := schemaPropertyNames(t, served, "served open_media_clip outputSchema")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("served open_media_clip output properties = %v, canonical = %v.\n"+
			"Any difference is drift: the canonical output object closes itself with "+
			"additionalProperties:false, so a canonically validating client rejects the "+
			"whole call on an undeclared field (#850).", got, want)
	}
}

// TestOpenMediaClip_RejectsASizeArgument_878 pins the gate on observable
// behavior, not only on a schema literal. A caller who has read #878 and tries
// the obvious argument must be told it does not exist, rather than have it
// silently ignored.
func TestOpenMediaClip_RejectsASizeArgument_878(t *testing.T) {
	t.Parallel()
	cfg, opts, chunkID := seedMediaClipCorpus(t, "clip.mp4", "video")
	srv := newServer(t, cfg, opts...)
	defer srv.Close()

	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	for _, property := range []string{"max_bytes", "quality", "rendition"} {
		args := `{"chunk_id":` + strconv.FormatInt(chunkID, 10) + `,"` + property + `":1}`
		envelope := callToolRaw(t, mcpURL, sid, "dir2mcp_open_media_clip", args)
		assertUnknownArgument(t, "dir2mcp_open_media_clip", property, envelope)
	}
}
