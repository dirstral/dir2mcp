package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// The WIRE half of issue #856, on the production SDK transport.
//
// PR #857 fixed the SELECTION: an impossible filter value now returns zero hits.
// It did not fix the VISIBILITY, because a served span carried only kind,
// start_ms and end_ms. So a caller who asked for events: ["home_run"] and got
// five hits could not confirm that all five carry that event, nor tell a
// filtered result from an unfiltered one. That is #856's first acceptance
// criterion, and it is why the original defect went unnoticed: no client could
// look.
//
// dirstral-spec 0.51.0 (df-005 0.2.0) permits `entities` and `event` on a time
// span, so the server can now show WHY a hit matched. These tests read the
// payload a real client gets, and validate it against the canonical
// common.json inside the pinned submodule, so prose and schema cannot drift.

// canonicalCommonSchemaPath is the pinned canonical shared-types schema inside
// the dirstral-spec submodule. The suite reads THAT file, not a copy, so a
// spec-side change reaches this test through the submodule pin.
var canonicalCommonSchemaPath = filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "common.json")

// spanVectorDim856 is the width of the toy embedding space these tests index in.
const spanVectorDim856 = 2

// spanStaticEmbedder856 returns one fixed vector for every text, so ranking is
// constant and a missing or extra hit can only be the filter's work.
type spanStaticEmbedder856 struct{}

func (spanStaticEmbedder856) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for range texts {
		out = append(out, []float32{1, 0})
	}
	return out, nil
}

// canonicalSpanRaw returns the raw `definitions.Span` subschema of the canonical
// common.json. That subschema is the contract for every served span.
func canonicalSpanRaw(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(canonicalCommonSchemaPath)
	if err != nil {
		t.Fatalf("read canonical common.json (run: git submodule update --init): %v", err)
	}
	var doc struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical common.json: %v", err)
	}
	span, ok := doc.Definitions["Span"]
	if !ok {
		t.Fatal("canonical common.json declares no definitions.Span")
	}
	return span
}

// canonicalSpanValidator compiles the canonical Span subschema into a validator.
func canonicalSpanValidator(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(canonicalSpanRaw(t), &schema); err != nil {
		t.Fatalf("decode canonical Span subschema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve canonical Span subschema: %v", err)
	}
	return resolved
}

// timeBranch856 picks the `time` member out of a Span schema's oneOf list. Both
// the canonical file and the served schema are compared through it, so neither
// side depends on the order the branches happen to be written in.
func timeBranch856(t *testing.T, span map[string]interface{}, label string) map[string]interface{} {
	t.Helper()
	branches, ok := span["oneOf"].([]interface{})
	if !ok {
		t.Fatalf("%s Span schema has no oneOf list: %#v", label, span["oneOf"])
	}
	for _, raw := range branches {
		branch, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		properties, ok := branch["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		kind, ok := properties["kind"].(map[string]interface{})
		if !ok {
			continue
		}
		if kind["const"] == "time" {
			return branch
		}
	}
	t.Fatalf("%s Span schema declares no time branch", label)
	return nil
}

// recognitionCorpus856 seeds one recognition annotation per event plus a prose
// note that mentions "home run" but carries no attribution. The note is the
// candidate an event filter must drop, and it is also the span that must NOT
// grow the new fields.
func recognitionCorpus856(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()

	seed := func(relPath, docType, repType, text string, span model.Span) {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath: relPath, DocType: docType, SourceType: "local", Status: "ok",
		}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", relPath, err)
		}
		doc, err := st.GetDocumentByPath(ctx, relPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
		}
		repID, err := st.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: repType, RepHash: relPath + repType, CreatedUnix: 1,
		})
		if err != nil {
			t.Fatalf("UpsertRepresentation(%s): %v", relPath, err)
		}
		if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: text,
			IndexKind: "text", EmbeddingStatus: "ok",
		}, []model.Span{span}); err != nil {
			t.Fatalf("InsertChunkWithSpans(%s): %v", relPath, err)
		}
	}

	seed("game.mp4", "video", "recognition",
		"Heliot Ramos hits a home run to left field",
		model.Span{
			Kind: "time", StartMS: 3346398, EndMS: 3354398,
			Entities: []string{"player:heliot-ramos", "team:san-francisco-giants"},
			Event:    "home_run",
		})
	seed("game2.mp4", "video", "recognition",
		"Logan Webb throws a home run ball on a pitch to the plate",
		model.Span{
			Kind: "time", StartMS: 120000, EndMS: 128000,
			Entities: []string{"player:logan-webb"},
			Event:    "pitch",
		})
	seed("notes.md", "md", "raw_text",
		"match report: the home run in the seventh decided the game",
		model.Span{Kind: "lines", StartLine: 1, EndLine: 2})
}

// recognitionServer856 wires the production chain behind the production SDK
// transport: a real SQLite store (spans + FTS), a real vector index, the real
// retrieval service, and the tools that advertise the filters.
func recognitionServer856(t *testing.T) (*runningServer, config.Config) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	recognitionCorpus856(t, st)

	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(st, idx, spanStaticEmbedder856{}, nil)

	// The daemon's warm-load path: read each embedded chunk's metadata back out
	// of the store and register it, then index its vector. The attribution
	// therefore reaches serialization exactly the way it does after a restart.
	tasks, err := st.ListEmbeddedChunkMetadata(ctx, "text", 100, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("warm load returned %d chunks, want 3", len(tasks))
	}
	for i, task := range tasks {
		meta := task.Metadata
		vec := make([]float32, spanVectorDim856)
		vec[0] = 1
		vec[1] = float32(i) / 1000
		if err := idx.Upsert(ctx, vec, model.IndexPayload{
			ChunkID: meta.ChunkID, RelPath: meta.RelPath, DocType: meta.DocType,
			RepType: meta.RepType, Span: meta.Span,
			StartMS: meta.Span.StartMS, EndMS: meta.Span.EndMS,
		}); err != nil {
			t.Fatalf("Upsert(%d): %v", meta.ChunkID, err)
		}
		svc.SetChunkMetadata(meta.ChunkID, meta.ToSearchHit())
	}

	cfg := defaultConfig()
	cfg.StateDir = tmp
	// newServerWithRetriever registers its own cleanup, so the caller does not.
	return newServerWithRetriever(t, cfg, svc, mcp.WithStore(st)), cfg
}

// callToolStructured856 calls one tool over the production transport and returns
// the structuredContent a real client validates.
func callToolStructured856(t *testing.T, srv *runningServer, cfg config.Config, toolName, arguments string) map[string]interface{} {
	t.Helper()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + toolName + `","arguments":` + arguments + `}}`
	resp := sendRPC(t, mcpURL, sid, body, nil)
	payload := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d want=200 body=%s", toolName, resp.StatusCode, payload)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("%s: decode response: %v body=%s", toolName, err, payload)
	}
	if envelope.Result.IsError {
		t.Fatalf("%s: isError=true body=%s", toolName, payload)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("%s: missing structuredContent body=%s", toolName, payload)
	}
	return envelope.Result.StructuredContent
}

// spansOf856 pulls the span object out of every entry of a structuredContent
// array field (hits or citations).
func spansOf856(t *testing.T, structured map[string]interface{}, field string) []map[string]interface{} {
	t.Helper()
	entries, ok := structured[field].([]interface{})
	if !ok {
		t.Fatalf("structuredContent has no %s array: %#v", field, structured[field])
	}
	out := make([]map[string]interface{}, 0, len(entries))
	for i, raw := range entries {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("%s[%d] is not an object: %#v", field, i, raw)
		}
		span, ok := entry["span"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s[%d] carries no span object: %#v", field, i, entry["span"])
		}
		out = append(out, span)
	}
	return out
}

// assertShowsHomeRun856 is the assertion that matters for #856: a hit returned
// under events: ["home_run"] must SHOW that event on its span, and must show the
// entity ids the recognizer attributed.
func assertShowsHomeRun856(t *testing.T, span map[string]interface{}, label string) {
	t.Helper()
	if span["event"] != "home_run" {
		pretty, _ := json.MarshalIndent(span, "", "  ")
		t.Fatalf("%s was selected by events:[\"home_run\"] but its span does not show that event, so a caller cannot tell a filtered result from an unfiltered one:\n%s", label, pretty)
	}
	entities, ok := span["entities"].([]interface{})
	if !ok {
		pretty, _ := json.MarshalIndent(span, "", "  ")
		t.Fatalf("%s span carries no entities array:\n%s", label, pretty)
	}
	want := map[string]bool{"player:heliot-ramos": false, "team:san-francisco-giants": false}
	for _, raw := range entities {
		id, _ := raw.(string)
		if _, tracked := want[id]; tracked {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("%s span.entities is missing %q: got %#v", label, id, entities)
		}
	}
}

// TestSearch_FilteredHitShowsTheEventItMatched is #856's first acceptance
// criterion on the wire. It FAILS on main: the served span carries only kind,
// start_ms and end_ms, so span["event"] is nil.
func TestSearch_FilteredHitShowsTheEventItMatched(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer856(t)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameSearch,
		`{"query":"home run","events":["home_run"],"k":20}`)
	spans := spansOf856(t, structured, "hits")
	if len(spans) != 1 {
		t.Fatalf("events:[\"home_run\"] returned %d hits, want 1 (the annotation only)", len(spans))
	}
	assertShowsHomeRun856(t, spans[0], "the dir2mcp_search hit")
}

// TestAsk_CitationSpanShowsTheAttribution covers the other tool that serves a
// Span. #849 exists because five tools disagreed with their own schemas, so a
// fix that only reaches dir2mcp_search is not a fix.
func TestAsk_CitationSpanShowsTheAttribution(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer856(t)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameAsk,
		`{"question":"who hit a home run?","events":["home_run"],"k":20}`)
	for _, field := range []string{"hits", "citations"} {
		spans := spansOf856(t, structured, field)
		if len(spans) != 1 {
			t.Fatalf("dir2mcp_ask returned %d %s, want 1", len(spans), field)
		}
		assertShowsHomeRun856(t, spans[0], "the dir2mcp_ask "+field+" entry")
	}
}

// TestSpan_NonRecognitionSpanCarriesNoAttribution is the compatibility guard.
// df-005 makes the three fields additive: a chunk that is not a recognition
// annotation carries none of them, so an existing consumer sees no change.
func TestSpan_NonRecognitionSpanCarriesNoAttribution(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer856(t)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameSearch,
		`{"query":"home run","k":20}`)
	spans := spansOf856(t, structured, "hits")
	if len(spans) != 3 {
		t.Fatalf("an unfiltered search returned %d hits, want all 3", len(spans))
	}
	var checked int
	for _, span := range spans {
		if span["kind"] != "lines" {
			continue
		}
		checked++
		for _, field := range []string{"entities", "event", "derivation", "sources"} {
			if _, present := span[field]; present {
				t.Errorf("a non-recognition span carries %q: %#v", field, span)
			}
		}
	}
	if checked != 1 {
		t.Fatalf("the prose note's lines span was not in the result, so this guard checked nothing: %#v", spans)
	}
}

// TestSpan_ServedSpansValidateAgainstCanonicalSchema validates every span a
// client actually receives against the canonical Span in the PINNED submodule.
// PR #852 did exactly this for stats and found a real break: prose and schema
// cannot drift apart while this test reads the canonical file.
//
// It is the guard that makes the emission safe. Every Span branch is
// additionalProperties:false, so a field the server invents but the contract
// does not declare makes a strict client reject the whole tool result: the #397
// "Failed to call tool" class.
func TestSpan_ServedSpansValidateAgainstCanonicalSchema(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer856(t)
	validator := canonicalSpanValidator(t)

	cases := []struct {
		tool, arguments, field string
	}{
		{protocol.ToolNameSearch, `{"query":"home run","k":20}`, "hits"},
		{protocol.ToolNameSearch, `{"query":"home run","events":["home_run"],"k":20}`, "hits"},
		{protocol.ToolNameAsk, `{"question":"who hit a home run?","k":20}`, "hits"},
		{protocol.ToolNameAsk, `{"question":"who hit a home run?","k":20}`, "citations"},
	}
	for _, tc := range cases {
		structured := callToolStructured856(t, srv, cfg, tc.tool, tc.arguments)
		spans := spansOf856(t, structured, tc.field)
		if len(spans) == 0 {
			t.Fatalf("%s %s returned no %s, so this check would be vacuous", tc.tool, tc.arguments, tc.field)
		}
		for i, span := range spans {
			if err := validator.Validate(span); err != nil {
				pretty, _ := json.MarshalIndent(span, "", "  ")
				t.Errorf("%s %s[%d] is invalid against the canonical common.json Span: %v\nspan:\n%s", tc.tool, tc.field, i, err, pretty)
			}
		}
	}
}

// TestSpan_CanonicalValidatorRejectsAnUndeclaredField is the negative control
// for the check above. Validating against the canonical file only means
// something if the validator really rejects a field the contract does not
// declare. Without this, a validator that resolved to "accept anything" would
// make every canonical check in this file pass vacuously.
//
// It uses `confidence` as the undeclared field on purpose. The recognize wire
// carries a per-annotation confidence (design 0004 §5) and the canonical time
// branch does not declare it, so it is the shape of a real mistake rather than a
// nonsense token: the branch is additionalProperties:false, and a span carrying
// it would make a strict client reject the whole tool result.
//
// It used to use `sources`. dirstral-spec 0.52.0 (df-005 0.3.0) declares that
// field, and #861 now serves it, so it is no longer an undeclared field and
// would make this control pass for the wrong reason.
func TestSpan_CanonicalValidatorRejectsAnUndeclaredField(t *testing.T) {
	t.Parallel()
	validator := canonicalSpanValidator(t)

	conforming := map[string]interface{}{
		"kind": "time", "start_ms": float64(0), "end_ms": float64(1000),
		"entities": []interface{}{"player:heliot-ramos"}, "event": "home_run",
		"sources": []interface{}{"scorebug"},
	}
	if err := validator.Validate(conforming); err != nil {
		t.Fatalf("the canonical Span validator rejected a conforming time span: %v", err)
	}

	undeclared := map[string]interface{}{
		"kind": "time", "start_ms": float64(0), "end_ms": float64(1000),
		"confidence": float64(0.97),
	}
	if err := validator.Validate(undeclared); err == nil {
		t.Fatal("the canonical Span validator accepted an undeclared `confidence` field, so every canonical check in this file would pass vacuously")
	}
}

// TestSpan_AdvertisedTimeBranchMatchesCanonical pins the other direction. The
// payload check above only sees what this corpus happens to emit. This one reads
// the schema the server publishes through tools/list and requires the same
// property set and required list on the time branch as the canonical file.
//
// It fails on main, where the served time branch declares neither `entities`
// nor `event`. A served branch that omits a canonical field is a real defect on
// its own: additionalProperties is false, so a client that validates against the
// SERVED schema rejects a conforming response from any implementation that
// emits the field.
func TestSpan_AdvertisedTimeBranchMatchesCanonical(t *testing.T) {
	t.Parallel()
	served, ok := toolsListSchemas(t)[protocol.ToolNameSearch].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/list advertises no outputSchema for %s", protocol.ToolNameSearch)
	}
	definitions, ok := served["definitions"].(map[string]interface{})
	if !ok {
		t.Fatalf("served %s outputSchema declares no definitions: %#v", protocol.ToolNameSearch, served["definitions"])
	}
	servedSpan, ok := definitions["Span"].(map[string]interface{})
	if !ok {
		t.Fatalf("served outputSchema declares no definitions.Span: %#v", definitions["Span"])
	}

	var canonicalSpan map[string]interface{}
	if err := json.Unmarshal(canonicalSpanRaw(t), &canonicalSpan); err != nil {
		t.Fatalf("decode canonical Span subschema as a tree: %v", err)
	}

	canonicalTime := timeBranch856(t, canonicalSpan, "canonical")
	servedTime := timeBranch856(t, servedSpan, "served")

	assertSameStrings(t, "common.json Span", "time-branch property",
		schemaPropertyNames(t, canonicalTime, "canonical time branch"),
		schemaPropertyNames(t, servedTime, "served time branch"))
	assertSameStrings(t, "common.json Span", "time-branch required field",
		schemaRequired(t, canonicalTime, "canonical time branch"),
		schemaRequired(t, servedTime, "served time branch"))
}
