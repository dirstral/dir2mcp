package conformance

// dir2mcp_open_media_clip cut at the SOURCE bitrate, so an 8 second clip of a
// 20 Mbit/s recording was about 22 MB, and about 30 MB base64 on the wire
// (#878). A pilot viewer read that latency as a broken play button.
//
// dirstral-spec 0.54.0 opened the contract this file used to pin shut, and
// these tests now pin the OPENED contract instead, still on the schema inside
// the dirstral-spec submodule rather than on a copy, so a spec-side change
// reaches them through the submodule pin (the
// tests/conformance/stats_canonical_schema_850_test.go idiom):
//
//   - `max_bytes` (input): the caller's ceiling on the CLIP bytes; the
//     effective bound is min(max_bytes, media.clip.max_bytes).
//   - `preview` (output): present exactly when the served bytes are a
//     reduced-fidelity re-encode. PRESENCE is the signal, mirroring
//     reference_fallback, so a preview is never mistakable for a source cut.
//
// Both objects stay closed with `additionalProperties: false`, so any OTHER
// size/quality argument (`quality`, `rendition`) is still rejected, and any
// undeclared output field is still drift (#850).

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

// TestOpenMediaClip_CanonicalInputCarriesMaxBytes_878 pins the input half of
// the opened contract: max_bytes is declared, and the object stays closed so no
// OTHER size argument can appear undeclared.
func TestOpenMediaClip_CanonicalInputCarriesMaxBytes_878(t *testing.T) {
	t.Parallel()
	input := canonicalOpenMediaClipSection(t, "input")
	assertClosedObject(t, input, "canonical open_media_clip input")

	want := []string{"chunk_id", "end_ms", "max_bytes", "rel_path", "return", "start_ms"}
	got := schemaPropertyNames(t, input, "canonical open_media_clip input")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical open_media_clip input properties = %v, want %v (spec 0.54.0)", got, want)
	}
}

// TestOpenMediaClip_CanonicalOutputNamesAPreview_878 pins the output half of
// the opened contract: preview is declared, and the object stays closed so a
// server cannot report fidelity through any undeclared side channel.
func TestOpenMediaClip_CanonicalOutputNamesAPreview_878(t *testing.T) {
	t.Parallel()
	output := canonicalOpenMediaClipSection(t, "output")
	assertClosedObject(t, output, "canonical open_media_clip output")

	want := []string{
		"data", "doc_type", "duration_ms", "expires_unix",
		"mime_type", "preview", "reference_fallback", "rel_path", "return",
		"size_bytes", "span", "uri",
	}
	got := schemaPropertyNames(t, output, "canonical open_media_clip output")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical open_media_clip output properties = %v, want %v (spec 0.54.0)", got, want)
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

// TestOpenMediaClip_RejectsAnUndeclaredSizeArgument_878 pins the closure on
// observable behavior, not only on a schema literal. max_bytes is real now, so
// only the arguments the contract still does NOT declare are rejected as
// unknown; a malformed max_bytes is rejected as INVALID_FIELD rather than
// silently ignored, because a typo that quietly disables the budget would hand
// the caller the 22 MB clip it asked not to receive.
func TestOpenMediaClip_RejectsAnUndeclaredSizeArgument_878(t *testing.T) {
	t.Parallel()
	cfg, opts, chunkID := seedMediaClipCorpus(t, "clip.mp4", "video")
	srv := newServer(t, cfg, opts...)
	defer srv.Close()

	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	for _, property := range []string{"quality", "rendition"} {
		args := `{"chunk_id":` + strconv.FormatInt(chunkID, 10) + `,"` + property + `":1}`
		envelope := callToolRaw(t, mcpURL, sid, "dir2mcp_open_media_clip", args)
		assertUnknownArgument(t, "dir2mcp_open_media_clip", property, envelope)
	}
	for _, bad := range []string{"0", "-1", "1.5", `"big"`} {
		args := `{"chunk_id":` + strconv.FormatInt(chunkID, 10) + `,"max_bytes":` + bad + `}`
		envelope := callToolRaw(t, mcpURL, sid, "dir2mcp_open_media_clip", args)
		errObj := envelope.Result.StructuredContent.Error
		if errObj == nil {
			t.Fatalf("max_bytes=%s accepted; a malformed budget must fail loudly", bad)
		}
		if errObj.Code != "INVALID_FIELD" {
			t.Fatalf("max_bytes=%s: code=%q message=%q, want INVALID_FIELD", bad, errObj.Code, errObj.Message)
		}
	}
}
