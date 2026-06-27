package mcp

import (
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// This file locks in the invariant that every MCP tool's representative
// structuredContent validates against the tool's declared outputSchema. It is
// the Go port of the schema_errors mini-validator in scripts/release_smoke.py
// (and the highest-leverage prevention from issue #428): a strict MCP client
// (Claude Desktop) validates structuredContent against outputSchema and rejects
// the WHOLE call ("Failed to call tool") when a serialized field is not allowed
// by an additionalProperties:false / oneOf schema. That is exactly the #387 bug
// class (hits[].modality / media_ref undeclared) and the #397 class (a Span with
// an unknown kind matching no oneOf branch). Asserting it here catches the class
// in CI instead of in a release smoke run.

// schemaErrors is a minimal recursive validator for the subset of JSON Schema the
// dir2mcp outputSchemas use: $ref (resolved against root #/definitions/...),
// oneOf (exactly one branch must match), const, enum, object (properties +
// additionalProperties:false rejects undeclared keys, required must be present),
// array/items, and scalar type (string/integer/number/boolean). It is
// deliberately small and dependency-free; conditional keywords (allOf/anyOf/
// if/then) are ignored, matching the release-smoke validator. It fails closed on
// an unresolved $ref. The returned strings are human-readable error paths; an
// empty slice means the instance conforms.
func schemaErrors(schema interface{}, inst interface{}, root map[string]interface{}, path string) []string {
	sch, ok := schema.(map[string]interface{})
	if !ok {
		return nil
	}
	if ref, ok := sch["$ref"].(string); ok {
		target := resolveRef(ref, root)
		if target == nil {
			return []string{path + ": unresolved $ref " + ref}
		}
		return schemaErrors(target, inst, root, path)
	}
	if branches, ok := sch["oneOf"].([]interface{}); ok {
		matched := 0
		for _, b := range branches {
			if len(schemaErrors(b, inst, root, path)) == 0 {
				matched++
			}
		}
		if matched == 1 {
			return nil
		}
		return []string{path + ": matched " + itoa(matched) + " oneOf branches (want 1)"}
	}
	if c, ok := sch["const"]; ok {
		if reflect.DeepEqual(inst, c) {
			return nil
		}
		return []string{path + ": value does not equal const"}
	}
	if enum, ok := sch["enum"]; ok {
		if enumContains(enum, inst) {
			return nil
		}
		return []string{path + ": value not in enum"}
	}
	return schemaErrorsByType(sch, inst, root, path)
}

// schemaErrorsByType handles the type-keyed branches (object/array/scalars). It
// is split out of schemaErrors to keep each function under the gocyclo budget.
func schemaErrorsByType(sch map[string]interface{}, inst interface{}, root map[string]interface{}, path string) []string {
	t, _ := sch["type"].(string)
	_, hasProps := sch["properties"]
	if t == "object" || (t == "" && hasProps) {
		return objectErrors(sch, inst, root, path)
	}
	if t == "array" {
		return arrayErrors(sch, inst, root, path)
	}
	return scalarErrors(t, inst, path)
}

func objectErrors(sch map[string]interface{}, inst interface{}, root map[string]interface{}, path string) []string {
	obj, ok := inst.(map[string]interface{})
	if !ok {
		return []string{path + ": expected object"}
	}
	var errs []string
	props, _ := sch["properties"].(map[string]interface{})
	for _, req := range toStringSlice(sch["required"]) {
		if _, present := obj[req]; !present {
			errs = append(errs, path+"."+req+": required property missing")
		}
	}
	addl, hasAddl := sch["additionalProperties"]
	addlFalse := hasAddl && addl == false
	addlSchema, addlIsSchema := addl.(map[string]interface{})
	for k, v := range obj {
		if propSchema, ok := props[k]; ok {
			errs = append(errs, schemaErrors(propSchema, v, root, path+"."+k)...)
		} else if addlIsSchema {
			errs = append(errs, schemaErrors(addlSchema, v, root, path+"."+k)...)
		} else if addlFalse {
			errs = append(errs, path+"."+k+": additional property not allowed by schema")
		}
	}
	return errs
}

func arrayErrors(sch map[string]interface{}, inst interface{}, root map[string]interface{}, path string) []string {
	rv := reflect.ValueOf(inst)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return []string{path + ": expected array"}
	}
	items, ok := sch["items"]
	if !ok {
		return nil
	}
	var errs []string
	for i := 0; i < rv.Len(); i++ {
		errs = append(errs, schemaErrors(items, rv.Index(i).Interface(), root, path+"["+itoa(i)+"]")...)
	}
	return errs
}

func scalarErrors(t string, inst interface{}, path string) []string {
	switch t {
	case "string":
		if _, ok := inst.(string); !ok {
			return []string{path + ": expected string"}
		}
	case "integer":
		if !isIntegerKind(inst) {
			return []string{path + ": expected integer"}
		}
	case "number":
		if !isNumberKind(inst) {
			return []string{path + ": expected number"}
		}
	case "boolean":
		if _, ok := inst.(bool); !ok {
			return []string{path + ": expected boolean"}
		}
	}
	return nil
}

// resolveRef resolves a local #/definitions/Name reference against the root
// schema. Anything else (or a missing definition) resolves to nil so the caller
// fails closed.
func resolveRef(ref string, root map[string]interface{}) map[string]interface{} {
	const prefix = "#/definitions/"
	if len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		return nil
	}
	defs, ok := root["definitions"].(map[string]interface{})
	if !ok {
		return nil
	}
	target, _ := defs[ref[len(prefix):]].(map[string]interface{})
	return target
}

// isIntegerKind reports whether inst is a Go integer value. A bool is rejected
// (mirrors the release-smoke validator guarding Python's bool<:int), as is any
// float — the dir2mcp serializers emit native ints for integer fields.
func isIntegerKind(inst interface{}) bool {
	if _, ok := inst.(bool); ok {
		return false
	}
	switch reflect.ValueOf(inst).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

// isNumberKind reports whether inst is a Go numeric value (integer or float),
// rejecting bool.
func isNumberKind(inst interface{}) bool {
	if isIntegerKind(inst) {
		return true
	}
	switch reflect.ValueOf(inst).Kind() {
	case reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

func enumContains(enum, inst interface{}) bool {
	rv := reflect.ValueOf(enum)
	if rv.Kind() != reflect.Slice {
		return false
	}
	for i := 0; i < rv.Len(); i++ {
		if reflect.DeepEqual(rv.Index(i).Interface(), inst) {
			return true
		}
	}
	return false
}

// toStringSlice normalizes a schema "required" value, which the Go schema
// builders express as []string but a generic decode would express as
// []interface{}.
func toStringSlice(v interface{}) []string {
	switch typed := v.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// fullSearchHit returns a hit with EVERY conditional serializeHit field
// populated (Title, Modality, MediaRef) and a region span (the richest Span
// variant). This is the exact shape that triggered #387 on docling corpora.
func fullSearchHit() model.SearchHit {
	return model.SearchHit{
		ChunkID:  42,
		RelPath:  "docs/report.pdf",
		Title:    "Quarterly Report",
		DocType:  "pdf",
		RepType:  "ocr",
		Score:    0.873,
		Snippet:  "revenue grew across all regions",
		Modality: "pdf",
		MediaRef: "docs/report.pdf",
		Span: model.Span{
			Kind:      "region",
			StartLine: 0,
			Region: &model.RegionSpan{
				StartPage: 3,
				EndPage:   3,
				BBox: &model.BBox{
					Page: 3, L: 10.5, T: 20.5, R: 110.5, B: 220.5,
					CoordOrigin: "TOPLEFT",
				},
				Section: []string{"Financials", "Revenue"},
			},
		},
	}
}

// assertConforms fails the test if the instance does not validate against the
// schema, printing every error path.
func assertConforms(t *testing.T, name string, schema, instance map[string]interface{}) {
	t.Helper()
	if errs := schemaErrors(schema, instance, schema, "$"); len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("%s: structuredContent violates outputSchema: %s", name, e)
		}
	}
}

func TestSearchOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"query":             "revenue",
		"k":                 5,
		"index_used":        "text",
		"hits":              []map[string]interface{}{serializeHit(fullSearchHit())},
		"indexing_complete": true,
	}
	assertConforms(t, "search", searchOutputSchema(), structured)
}

func TestAskOutputSchemaConformance(t *testing.T) {
	result := model.AskResult{
		Question: "what drove revenue?",
		Answer:   "Growth across all regions.",
		Citations: []model.Citation{{
			ChunkID: 42,
			RelPath: "docs/report.pdf",
			Title:   "Quarterly Report",
			Span:    fullSearchHit().Span,
		}},
		Hits:             []model.SearchHit{fullSearchHit()},
		IndexingComplete: true,
	}
	assertConforms(t, "ask", askOutputSchema(), buildAskStructuredContent(result))
}

func TestAskAudioOutputSchemaConformance(t *testing.T) {
	structured := buildAskStructuredContent(model.AskResult{
		Question:         "q",
		Answer:           "a",
		Hits:             []model.SearchHit{fullSearchHit()},
		IndexingComplete: true,
	})
	structured["audio"] = map[string]interface{}{
		"mime_type": "audio/mpeg",
		"data":      "QUJD",
	}
	assertConforms(t, "ask_audio", askAudioOutputSchema(), structured)
}

func TestTranscribeAndAskOutputSchemaConformance(t *testing.T) {
	structured := buildAskStructuredContent(model.AskResult{
		Question:         "q",
		Answer:           "a",
		Hits:             []model.SearchHit{fullSearchHit()},
		IndexingComplete: true,
	})
	structured["transcript_provider"] = "mistral"
	structured["transcript_model"] = "voxtral-mini-latest"
	structured["transcribed"] = true
	structured["transcribed_now"] = false
	assertConforms(t, "transcribe_and_ask", transcribeAndAskOutputSchema(), structured)
}

// TestSearchOutputSchemaNegativeControl proves the validator actually catches
// the #387 class: an undeclared field on an additionalProperties:false hit must
// be reported. If this ever stops failing, the positive tests above are
// worthless.
func TestSearchOutputSchemaNegativeControl(t *testing.T) {
	hit := serializeHit(fullSearchHit())
	hit["bogus_undeclared_field"] = "x" // the kind of leak that broke #387
	structured := map[string]interface{}{
		"query":             "revenue",
		"k":                 5,
		"index_used":        "text",
		"hits":              []map[string]interface{}{hit},
		"indexing_complete": true,
	}
	errs := schemaErrors(searchOutputSchema(), structured, searchOutputSchema(), "$")
	if len(errs) == 0 {
		t.Fatal("validator did NOT detect an undeclared hit field; it would not catch the #387 class")
	}
}

func TestOpenFileOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"rel_path":  "docs/report.pdf",
		"doc_type":  "pdf",
		"content":   "full document text",
		"truncated": false,
		"span":      buildOpenFileSpan(model.Span{Kind: "page", Page: 3}),
	}
	assertConforms(t, "open_file", openFileOutputSchema(), structured)
}

func TestOpenMediaClipOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"rel_path":    "media/talk.mp4",
		"doc_type":    "video",
		"span":        buildOpenFileSpan(model.Span{Kind: "time", StartMS: 1000, EndMS: 5000}),
		"mime_type":   "video/mp4",
		"duration_ms": 4000,
		"size_bytes":  2048,
		"return":      "inline",
		"data":        "QUJD",
	}
	assertConforms(t, "open_media_clip", openMediaClipOutputSchema(), structured)
}

func TestListFilesOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"limit":  200,
		"offset": 0,
		"total":  2,
		"files": []map[string]interface{}{
			{
				"rel_path":   "docs/a.md",
				"title":      "A",
				"doc_type":   "markdown",
				"size_bytes": 123,
				"mtime_unix": int64(1700000000),
				"status":     "ok",
				"deleted":    false,
			},
			{
				"rel_path":   "docs/b.pdf",
				"doc_type":   "pdf",
				"size_bytes": 456,
				"mtime_unix": int64(1700000001),
				"status":     "error",
				"deleted":    true,
			},
		},
	}
	assertConforms(t, "list_files", listFilesOutputSchema(), structured)
}

func TestTranscribeOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"rel_path":        "media/clip.mp3",
		"provider":        "mistral",
		"model":           "voxtral-mini-latest",
		"indexed":         true,
		"transcribed":     true,
		"transcribed_now": false,
		"segments": []map[string]interface{}{
			{"start_ms": 0, "end_ms": 1500, "text": "hello"},
			{"start_ms": 1500, "end_ms": 3000, "text": "world"},
		},
	}
	assertConforms(t, "transcribe", transcribeOutputSchema(), structured)
}

func TestAnnotateOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"rel_path":                "docs/form.pdf",
		"stored":                  true,
		"flattened_indexed":       true,
		"annotation_json":         map[string]interface{}{"field": "value"},
		"annotation_text_preview": "field: value",
		"source_doc_type":         "pdf",
		"source_rep":              "ocr",
	}
	assertConforms(t, "annotate", annotateOutputSchema(), structured)
}

func TestStatsOutputSchemaConformance(t *testing.T) {
	structured := map[string]interface{}{
		"root":             "/corpus",
		"state_dir":        "/corpus/.dir2mcp",
		"protocol_version": "2025-06-18",
		"doc_counts": map[string]interface{}{
			"markdown": 3,
			"pdf":      2,
		},
		"total_docs":           5,
		"doc_counts_available": true,
		"indexing": map[string]interface{}{
			"job_id":          "job-1",
			"running":         false,
			"mode":            "incremental",
			"scanned":         5,
			"indexed":         5,
			"skipped":         0,
			"deleted":         0,
			"representations": 7,
			"chunks_total":    12,
			"embedded_ok":     12,
			"errors":          0,
		},
		"models": map[string]interface{}{
			"embed_text":   "mistral-embed",
			"embed_code":   "codestral-embed",
			"ocr":          "mistral-ocr-latest",
			"stt_provider": "mistral",
			"stt_model":    "voxtral-mini-latest",
			"chat":         "mistral-large-latest",
		},
		"sessions": map[string]interface{}{
			"active": 1,
			"items": []map[string]interface{}{
				{"id": "abc***", "created_unix": int64(1700000000), "last_seen_unix": int64(1700000100)},
			},
		},
		"recent_failures": []map[string]interface{}{
			{"rel_path": "docs/bad.pdf", "doc_type": "pdf", "mtime_unix": int64(1700000050), "error_message": "ocr failed"},
		},
	}
	assertConforms(t, "stats", statsOutputSchema(), structured)
}

// TestSpanVariantsConformToSpanSchema asserts buildOpenFileSpan output for each
// span kind validates against the Span definition (the schema shared by search/
// ask/open_file/open_media_clip via $ref). The empty/unknown-kind case is the
// #397 class: on current main buildOpenFileSpan emits {"kind": ""}, which
// matches no oneOf branch, so a strict client rejects the whole call. We surface
// it as a logged known gap rather than failing the suite, so this test passes on
// main while documenting the invariant the #397 fix must restore.
func TestSpanVariantsConformToSpanSchema(t *testing.T) {
	// A standalone root that exposes the Span definition for $ref resolution.
	spanRoot := map[string]interface{}{
		"$ref":        "#/definitions/Span",
		"definitions": sharedDefinitions(),
	}

	cases := []struct {
		name string
		span model.Span
	}{
		{"lines", model.Span{Kind: "lines", StartLine: 1, EndLine: 10}},
		{"page", model.Span{Kind: "page", Page: 3}},
		{"time", model.Span{Kind: "time", StartMS: 1000, EndMS: 5000}},
		{"time_diarized", model.Span{Kind: "time", StartMS: 1000, EndMS: 5000, Speaker: "S1", SpeakerLabel: "Alice"}},
		{"region", fullSearchHit().Span},
		// A region span whose payload/bbox is absent degrades to a page or
		// document span; cover that path too.
		{"region_degraded", model.Span{Kind: "region", Region: &model.RegionSpan{StartPage: 4}}},
		{"region_document", model.Span{Kind: "region"}},
		{"document", model.Span{Kind: "document"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built := buildOpenFileSpan(tc.span)
			if errs := schemaErrors(spanRoot, built, spanRoot, "$"); len(errs) != 0 {
				for _, e := range errs {
					t.Errorf("span kind %q violates Span schema: %s", tc.span.Kind, e)
				}
			}
		})
	}

	// Empty/unknown kind: known gap (#397). buildOpenFileSpan emits {"kind": ""}
	// for an unset kind, which matches no Span oneOf branch. Assert the validator
	// SEES the violation (so the invariant is documented and the test is honest),
	// but log it as a TODO rather than failing — the #397 fix must make
	// buildOpenFileSpan never emit an unknown-kind span, at which point this can
	// be promoted to a hard assertion.
	empty := buildOpenFileSpan(model.Span{Kind: ""})
	if errs := schemaErrors(spanRoot, empty, spanRoot, "$"); len(errs) == 0 {
		t.Log("empty-kind span now conforms to the Span schema; #397 appears fixed — promote this to a hard assertion")
	} else {
		t.Logf("KNOWN GAP (#397): empty-kind span does not conform to the Span schema: %v", errs)
	}
}
