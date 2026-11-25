package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/editorial"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Conformance cases 6, 7 and 8 of SPEC §7.11.8 on the wire: an imported
// editorial annotation is applied by the ingest service, read back out of the
// store the way a daemon restart reads it, and served by the production MCP
// transport. All names and records are made up.

const edSource = "example-catalogue"

// editorialRecords is the import of this file: two spellings of one name on two
// documents, a third document that lists neither, and a record whose name is
// ъ U+0308 for the fold-loop case.
var editorialRecords = []string{
	`{"stem": "first", "match": {"score": 1.25, "external_id": "X-2002"}, "people": [{"name": "  ПЁТР   ЁЛКИН ", "role": "инженер"}], ` +
		`"segments": [{"t": "00:00:00", "text": "Вступление ведущего"}, {"t": "00:01:00", "text": "Прогноз погоды на неделю"}], "description": "Выпуск о погоде."}`,
	`{"stem": "second", "match": {"score": 0.5}, "people": [{"name": "Петр Елкин"}]}`,
	`{"stem": "third", "people": [{"name": "Анна Белова"}]}`,
	`{"stem": "fourth", "people": [{"name": "ъ̈"}]}`,
}

// editorialServer seeds four videos and one recognition annotation, applies the
// import with the given name folds, warms the retrieval service from the store
// and serves it over MCP.
func editorialServer(t *testing.T, folds []string) (*runningServer, config.Config) {
	t.Helper()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, name := range []string{"first.mp4", "second.mp4", "third.mp4", "fourth.mp4"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("media "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: name, DocType: "video", SourceType: "filesystem", Status: "ok", ContentHash: "h-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	seedRecognitionHit(t, st)
	applyEditorialImport(t, root, stateDir, st, folds)
	markAllEmbedded(t, st)

	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(st, idx, spanStaticEmbedder856{}, nil)
	if err := svc.SetEditorialNameFolds(folds); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListEmbeddedChunkMetadata(ctx, "text", 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		meta := task.Metadata
		vec := []float32{1, float32(i) / 1000}
		if err := idx.Upsert(ctx, vec, model.IndexPayload{
			ChunkID: meta.ChunkID, RelPath: meta.RelPath, DocType: meta.DocType, RepType: meta.RepType, Span: meta.Span,
			StartMS: meta.Span.StartMS, EndMS: meta.Span.EndMS,
		}); err != nil {
			t.Fatal(err)
		}
		svc.SetChunkMetadata(meta.ChunkID, meta.ToSearchHit())
	}
	cfg := defaultConfig()
	cfg.StateDir = stateDir
	return newServerWithRetriever(t, cfg, svc, mcp.WithStore(st)), cfg
}

// seedRecognitionHit stores one recognition annotation whose entity id is the
// literal name, so the case can show that recognition stays literal.
func seedRecognitionHit(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "recog.mp4", DocType: "video", SourceType: "filesystem", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	doc, err := st.GetDocumentByPath(ctx, "recog.mp4")
	if err != nil {
		t.Fatal(err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{DocID: doc.DocID, RepType: "recognition", RepHash: "r", CreatedUnix: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{RepID: repID, Text: "Пётр Ёлкин on screen", IndexKind: "text"},
		[]model.Span{{Kind: "time", StartMS: 1000, EndMS: 2000, Entities: []string{"Пётр Ёлкин"}, Event: "face", Sources: []string{"face"}}}); err != nil {
		t.Fatal(err)
	}
}

func applyEditorialImport(t *testing.T, root, stateDir string, st *store.SQLiteStore, folds []string) {
	t.Helper()
	batch, problems, err := editorial.ParseFile(strings.NewReader(strings.Join(editorialRecords, "\n")), editorial.KeyStem, edSource)
	if err != nil || len(problems) > 0 {
		t.Fatalf("ParseFile: %v %+v", err, problems)
	}
	now := time.Now()
	if _, err := editorial.Stage(stateDir, editorial.NewStaged(batch, edSource, editorial.KeyStem, now), now); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.AnnotationsNameFolds = folds
	svc, err := ingest.NewService(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 3 * time.Minute, nil }
	if err := svc.ApplyEditorialImports(context.Background()); err != nil {
		t.Fatalf("ApplyEditorialImports: %v", err)
	}
}

func markAllEmbedded(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	db, err := st.EnsureDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT chunk_id FROM chunks WHERE deleted = 0`)
	if err != nil {
		st.ReleaseDB()
		t.Fatal(err)
	}
	var labels []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, uint64(id))
	}
	_ = rows.Close()
	st.ReleaseDB()
	if err := st.MarkEmbedded(ctx, labels); err != nil {
		t.Fatal(err)
	}
}

// hitsOf returns the hit objects of a structured search or ask result.
func hitsOf(t *testing.T, structured map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := structured["hits"].([]interface{})
	if !ok {
		t.Fatalf("no hits array: %#v", structured["hits"])
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]interface{}))
	}
	return out
}

// editorialPaths lists the rel_paths of the editorial hits.
func editorialPaths(hits []map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	for _, h := range hits {
		if h["rep_type"] == "editorial" {
			out[h["rel_path"].(string)] = true
		}
	}
	return out
}

func searchHits(t *testing.T, srv *runningServer, cfg config.Config, args string) []map[string]interface{} {
	t.Helper()
	return hitsOf(t, callToolStructured856(t, srv, cfg, protocol.ToolNameSearch, args))
}

// Case 6, default folds: the two spellings are different ids, and recognition
// still compares literally.
func TestEditorialPersonFilter_DefaultFoldsNothing(t *testing.T) {
	t.Parallel()
	srv, cfg := editorialServer(t, nil)
	hits := searchHits(t, srv, cfg, `{"query":"Пётр Ёлкин","entities":["Пётр Ёлкин"],"k":50}`)
	if got := editorialPaths(hits); !reflect.DeepEqual(got, map[string]bool{"first.mp4": true}) {
		t.Fatalf("editorial hits come from %v, want first.mp4 only", got)
	}
	recognized := false
	for _, h := range hits {
		if h["rep_type"] == "recognition" {
			recognized = true
		}
	}
	if !recognized {
		t.Fatal("the literal value no longer matches the recognition hit")
	}
	// A normalized value matches the editorial id, and not the literal
	// recognition id.
	hits = searchHits(t, srv, cfg, `{"query":"Пётр Ёлкин","entities":["пётр ёлкин"],"k":50}`)
	for _, h := range hits {
		if h["rep_type"] == "recognition" {
			t.Fatal("a recognition hit matched a value that differs from its id")
		}
	}
	if !editorialPaths(hits)["first.mp4"] {
		t.Fatal("the normalized id does not match its editorial hit")
	}
}

// Case 6, ё=е: the fold makes both spellings one id, on dir2mcp_search and on
// dir2mcp_ask. The rebuild of stored ids after a folds change with no new
// import is pinned in tests/ingest (TestEditorialApply_RebuildsOnFoldsAndDuration).
func TestEditorialPersonFilter_FoldMergesSpellings(t *testing.T) {
	t.Parallel()
	srv, cfg := editorialServer(t, []string{"ё=е"})
	want := map[string]bool{"first.mp4": true, "second.mp4": true}
	hits := searchHits(t, srv, cfg, `{"query":"Пётр Ёлкин","entities":["Пётр Ёлкин"],"k":50}`)
	if got := editorialPaths(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("search: editorial hits come from %v, want first.mp4 and second.mp4", got)
	}
	asked := hitsOf(t, callToolStructured856(t, srv, cfg, protocol.ToolNameAsk, `{"question":"Пётр Ёлкин","entities":["Пётр Ёлкин"],"k":50}`))
	if got := editorialPaths(asked); !reflect.DeepEqual(got, want) {
		t.Fatalf("ask: editorial hits come from %v, want first.mp4 and second.mp4", got)
	}
}

// Case 6, ъ=е and ё=е: ъ U+0308 normalizes to е through the repeat loop, and a
// filter for that stored id returns its hit.
func TestEditorialPersonFilter_FoldLoop(t *testing.T) {
	t.Parallel()
	srv, cfg := editorialServer(t, []string{"ъ=е", "ё=е"})
	hits := searchHits(t, srv, cfg, `{"query":"е","entities":["е"],"k":50}`)
	if !editorialPaths(hits)["fourth.mp4"] {
		t.Fatalf("the stored id е does not return the fourth.mp4 hit: %v", editorialPaths(hits))
	}
	for _, h := range hits {
		span := h["span"].(map[string]interface{})
		if h["rel_path"] == "fourth.mp4" && !reflect.DeepEqual(span["entities"], []interface{}{"е"}) {
			t.Fatalf("stored id = %v, want [е]", span["entities"])
		}
	}
}

// Case 7: a query that matches one segment returns its hit with the segment's
// time range.
func TestEditorialSegmentQuery(t *testing.T) {
	t.Parallel()
	srv, cfg := editorialServer(t, nil)
	hits := searchHits(t, srv, cfg, `{"query":"Прогноз погоды на неделю","events":["segment"],"k":50}`)
	found := false
	for _, h := range hits {
		if !strings.Contains(h["snippet"].(string), "Прогноз погоды") {
			continue
		}
		found = true
		span := h["span"].(map[string]interface{})
		if span["kind"] != "time" || span["start_ms"] != float64(60000) || span["end_ms"] != float64(180000) {
			t.Fatalf("segment hit span = %v, want time [60000, 180000]", span)
		}
	}
	if !found {
		t.Fatal("the segment query returned no hit for the segment")
	}
}

// Case 8: provenance on every editorial hit, the source filter, the editor
// annotation citation, and the canonical Span schema.
func TestEditorialProvenanceAndSourceFilter(t *testing.T) {
	t.Parallel()
	srv, cfg := editorialServer(t, nil)
	validator := canonicalSpanValidator(t)
	hits := searchHits(t, srv, cfg, `{"query":"Пётр","sources":["`+edSource+`"],"k":50}`)
	if len(hits) == 0 {
		t.Fatal("the source filter returned nothing")
	}
	sawDocumentSpan := false
	for _, h := range hits {
		if assertEditorialHitProvenance(t, h, validator) {
			sawDocumentSpan = true
		}
	}
	if !sawDocumentSpan {
		t.Fatal("no description hit with a document span was served")
	}
	if got := searchHits(t, srv, cfg, `{"query":"Пётр","sources":[" `+edSource+`"],"k":50}`); len(got) != 0 {
		t.Fatalf("a source value with a leading space matched %d hits; the filter is literal", len(got))
	}
	if got := searchHits(t, srv, cfg, `{"query":"Пётр","sources":["face"],"k":50}`); len(got) != 1 || got[0]["rep_type"] != "recognition" {
		t.Fatalf("sources:[face] returned %d hits, want the one recognition hit", len(got))
	}

	text := searchContentText(t, srv, cfg, `{"query":"Прогноз","sources":["`+edSource+`"],"k":50}`)
	if !strings.Contains(text, "editor annotation ("+edSource+", match 1.25)") {
		t.Fatalf("the search text does not render the editor annotation label:\n%s", text)
	}
}

// assertEditorialHitProvenance checks one hit of the source filter and reports
// whether its span is a document span.
func assertEditorialHitProvenance(t *testing.T, h map[string]interface{}, validator interface{ Validate(any) error }) bool {
	t.Helper()
	span := h["span"].(map[string]interface{})
	if h["rep_type"] != "editorial" || !reflect.DeepEqual(span["sources"], []interface{}{edSource}) {
		t.Fatalf("the source filter returned a hit that is not from the source: %v", h)
	}
	if _, has := span["derivation"]; has {
		t.Fatalf("an editorial span carries derivation: %v", span)
	}
	if h["rel_path"] == "first.mp4" {
		attrs, _ := span["attributes"].(map[string]interface{})
		if attrs["match_score"] != "1.25" || attrs["external_id"] != "X-2002" {
			t.Fatalf("first.mp4 hit attributes = %v", attrs)
		}
	}
	if err := validator.Validate(span); err != nil {
		t.Fatalf("served editorial span is invalid against the canonical Span: %v\n%v", err, span)
	}
	return span["kind"] == "document"
}

// TestEditorialCitationForm pins the SPEC §9.3 citation forms.
func TestEditorialCitationForm(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{"match_score": "1.25"}
	timed := model.Span{Kind: "time", StartMS: 4000, EndMS: 72000, Sources: []string{edSource}, Attributes: attrs}
	if got := retrieval.FormatHitCitation("news.mp4", "editorial", timed); got != "[news.mp4@t=00:04-01:12 › editor annotation (example-catalogue, match 1.25)]" {
		t.Fatalf("time form = %q", got)
	}
	doc := model.Span{Kind: "document", Sources: []string{edSource}}
	if got := retrieval.FormatHitCitation("news.mp4", "editorial", doc); got != "[news.mp4 › editor annotation (example-catalogue)]" {
		t.Fatalf("document form without a score = %q", got)
	}
	if got := retrieval.FormatHitCitation("news.mp4", "transcript", timed); got != "[news.mp4@t=00:04-01:12]" {
		t.Fatalf("a transcript citation changed: %q", got)
	}
}

// searchContentText returns the text content block of a dir2mcp_search call.
func searchContentText(t *testing.T, srv *runningServer, cfg config.Config, arguments string) string {
	t.Helper()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + protocol.ToolNameSearch + `","arguments":` + arguments + `}}`
	resp := sendRPC(t, mcpURL, sid, body, nil)
	payload := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, payload)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Result.Content) == 0 {
		t.Fatalf("decode content: %v body=%s", err, payload)
	}
	return envelope.Result.Content[0].Text
}
