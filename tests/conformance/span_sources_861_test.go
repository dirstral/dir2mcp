package conformance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// The WIRE half of issue #861, on the production SDK transport.
//
// A recognition annotation is produced by one or more recognizers: a
// play-by-play feed, a scorebug reader, a face matcher. The backend has always
// sent those tags, ingestion dropped them, and a viewer asking "how do you know"
// could not be told which component spoke. dirstral-spec 0.52.0 (df-005 0.3.0)
// puts `sources` on the `time` span, so the server can now say so.
//
// df-005 is explicit that `sources` is PROVENANCE ONLY: an implementation MUST
// NOT require a client to read it to rank or filter correctly. The last test in
// this file is that rule, pinned as a byte-for-byte comparison.

// sources861 is the tag list the seeded annotations carry. Two tags, in a fixed
// order, so a served span can be checked for order as well as content.
var sources861 = []string{"playbyplay", "scorebug"}

// recognitionCorpus861 seeds the same three documents as the #856 corpus, with
// the recognizer tags under test on the two annotations. Passing nil sources
// seeds the identical corpus WITHOUT provenance, which is the control the
// provenance-only test compares against.
func recognitionCorpus861(t *testing.T, st *store.SQLiteStore, sources []string) {
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
			Entities: []string{"player:heliot-ramos"},
			Event:    "home_run",
			Sources:  sources,
		})
	seed("game2.mp4", "video", "recognition",
		"Logan Webb throws a home run ball on a pitch to the plate",
		model.Span{
			Kind: "time", StartMS: 120000, EndMS: 128000,
			Entities: []string{"player:logan-webb"},
			Event:    "pitch",
			Sources:  sources,
		})
	seed("notes.md", "md", "raw_text",
		"match report: the home run in the seventh decided the game",
		model.Span{Kind: "lines", StartLine: 1, EndLine: 2})
}

// recognitionServer861 wires the production chain behind the production SDK
// transport, over a corpus seeded with (or without) recognizer tags. The warm
// load reads each chunk back out of the store, so the provenance reaches
// serialization exactly the way it does after a daemon restart.
func recognitionServer861(t *testing.T, sources []string) (*runningServer, config.Config) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	recognitionCorpus861(t, st, sources)

	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(st, idx, spanStaticEmbedder856{}, nil)

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
	return newServerWithRetriever(t, cfg, svc, mcp.WithStore(st)), cfg
}

// assertSources861 requires a span to name its recognizers, in the order the
// producer named them.
func assertSources861(t *testing.T, span map[string]interface{}, label string) {
	t.Helper()
	raw, ok := span["sources"].([]interface{})
	if !ok {
		pretty, _ := json.MarshalIndent(span, "", "  ")
		t.Fatalf("%s carries no sources array, so a caller cannot learn which recognizer produced it:\n%s", label, pretty)
	}
	if len(raw) != len(sources861) {
		t.Fatalf("%s span.sources = %#v, want %v", label, raw, sources861)
	}
	for i, want := range sources861 {
		if raw[i] != want {
			t.Fatalf("%s span.sources[%d] = %#v, want %q (order is the producer's)", label, i, raw[i], want)
		}
	}
}

// TestSearch_HitNamesTheRecognizersBehindTheAnnotation is #861's acceptance
// criterion on the wire. It FAILS before the fix: ingestion dropped the tags, so
// the served span carries no `sources` at all.
func TestSearch_HitNamesTheRecognizersBehindTheAnnotation(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer861(t, sources861)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameSearch,
		`{"query":"home run","events":["home_run"],"k":20}`)
	spans := spansOf856(t, structured, "hits")
	if len(spans) != 1 {
		t.Fatalf("events:[\"home_run\"] returned %d hits, want 1 (the annotation only)", len(spans))
	}
	assertSources861(t, spans[0], "the dir2mcp_search hit")
}

// TestAsk_CitationSpanNamesTheRecognizers covers the other tool that serves a
// Span. #849 exists because five tools disagreed with their own schemas, so a
// fix that only reaches dir2mcp_search is not a fix.
func TestAsk_CitationSpanNamesTheRecognizers(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer861(t, sources861)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameAsk,
		`{"question":"who hit a home run?","events":["home_run"],"k":20}`)
	for _, field := range []string{"hits", "citations"} {
		spans := spansOf856(t, structured, field)
		if len(spans) != 1 {
			t.Fatalf("dir2mcp_ask returned %d %s, want 1", len(spans), field)
		}
		assertSources861(t, spans[0], "the dir2mcp_ask "+field+" entry")
	}
}

// TestSpan_NoSourcesOmitsTheFieldEntirely is the optionality rule. df-005 makes
// `sources` optional, so an annotation that names no recognizer must omit the
// key: not `null`, not `[]`. A decoded payload distinguishes all three, because
// both of the wrong shapes leave the key present.
func TestSpan_NoSourcesOmitsTheFieldEntirely(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer861(t, nil)

	structured := callToolStructured856(t, srv, cfg, protocol.ToolNameSearch,
		`{"query":"home run","k":20}`)
	spans := spansOf856(t, structured, "hits")
	if len(spans) != 3 {
		t.Fatalf("an unfiltered search returned %d hits, want all 3", len(spans))
	}
	for _, span := range spans {
		if value, present := span["sources"]; present {
			pretty, _ := json.MarshalIndent(span, "", "  ")
			t.Errorf("a span with no recognizer tags still carries sources=%#v:\n%s", value, pretty)
		}
	}
}

// TestSpan_ServedSourcesValidateAgainstCanonicalSchema validates the spans a
// client actually receives against the canonical Span in the PINNED submodule.
// Every Span branch is additionalProperties:false, so serving a field the
// contract does not declare makes a strict client reject the whole tool result:
// the #397 "Failed to call tool" class. This is the guard that makes emitting
// `sources` safe, and it only passes on a submodule pinned at 0.52.0 or later.
func TestSpan_ServedSourcesValidateAgainstCanonicalSchema(t *testing.T) {
	t.Parallel()
	srv, cfg := recognitionServer861(t, sources861)
	validator := canonicalSpanValidator(t)

	cases := []struct {
		tool, arguments, field string
	}{
		{protocol.ToolNameSearch, `{"query":"home run","k":20}`, "hits"},
		{protocol.ToolNameAsk, `{"question":"who hit a home run?","k":20}`, "hits"},
		{protocol.ToolNameAsk, `{"question":"who hit a home run?","k":20}`, "citations"},
	}
	var checked int
	for _, tc := range cases {
		structured := callToolStructured856(t, srv, cfg, tc.tool, tc.arguments)
		spans := spansOf856(t, structured, tc.field)
		if len(spans) == 0 {
			t.Fatalf("%s %s returned no %s, so this check would be vacuous", tc.tool, tc.arguments, tc.field)
		}
		for i, span := range spans {
			if _, present := span["sources"]; present {
				checked++
			}
			if err := validator.Validate(span); err != nil {
				pretty, _ := json.MarshalIndent(span, "", "  ")
				t.Errorf("%s %s[%d] is invalid against the canonical common.json Span: %v\nspan:\n%s", tc.tool, tc.field, i, err, pretty)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no served span carried sources, so this check validated nothing")
	}
}

// withoutSources returns a deep copy of a decoded payload with the `sources` key
// removed from every span object. A span is identified by its own required
// fields (kind + start_ms), so an unrelated field of the same name elsewhere in
// a payload is left alone and the comparison stays honest.
func withoutSources(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			out[key] = withoutSources(child)
		}
		_, isSpan := out["kind"]
		if _, hasStart := out["start_ms"]; isSpan && hasStart {
			delete(out, "sources")
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(typed))
		for _, child := range typed {
			out = append(out, withoutSources(child))
		}
		return out
	default:
		return value
	}
}

// countSources reports how many span objects in a payload carry `sources`.
func countSources(value interface{}) int {
	switch typed := value.(type) {
	case map[string]interface{}:
		total := 0
		for _, child := range typed {
			total += countSources(child)
		}
		if _, present := typed["sources"]; present {
			total++
		}
		return total
	case []interface{}:
		total := 0
		for _, child := range typed {
			total += countSources(child)
		}
		return total
	default:
		return 0
	}
}

// TestSources_AreProvenanceOnly pins the rule df-005 0.3.0 states: an
// implementation MUST NOT require a client to read `sources` to rank or filter
// correctly. Two corpora that differ ONLY in the recognizer tags must answer
// every query identically: same hits, same order, same scores, same citations.
//
// The comparison is byte-for-byte on the served payload with the tags removed,
// so a scoring, filtering, dedup or citation rule that started to read the tags
// fails here even if it kept the hit count the same.
func TestSources_AreProvenanceOnly(t *testing.T) {
	t.Parallel()
	tagged, taggedCfg := recognitionServer861(t, sources861)
	plain, plainCfg := recognitionServer861(t, nil)

	cases := []struct{ tool, arguments string }{
		{protocol.ToolNameSearch, `{"query":"home run","k":20}`},
		{protocol.ToolNameSearch, `{"query":"home run","events":["home_run"],"k":20}`},
		{protocol.ToolNameSearch, `{"query":"home run","entities":["player:logan-webb"],"k":20}`},
		{protocol.ToolNameAsk, `{"question":"who hit a home run?","k":20}`},
	}
	for _, tc := range cases {
		withTags := callToolStructured856(t, tagged, taggedCfg, tc.tool, tc.arguments)
		withoutTags := callToolStructured856(t, plain, plainCfg, tc.tool, tc.arguments)

		if countSources(withTags) == 0 {
			t.Fatalf("%s %s served no sources at all, so this comparison would be vacuous", tc.tool, tc.arguments)
		}
		if got := countSources(withoutTags); got != 0 {
			t.Fatalf("%s %s served %d sources from a corpus that has none", tc.tool, tc.arguments, got)
		}

		strippedTagged, err := json.Marshal(withoutSources(withTags))
		if err != nil {
			t.Fatalf("marshal tagged payload: %v", err)
		}
		strippedPlain, err := json.Marshal(withoutSources(withoutTags))
		if err != nil {
			t.Fatalf("marshal untagged payload: %v", err)
		}
		if string(strippedTagged) != string(strippedPlain) {
			t.Errorf("%s %s answers differently once recognizer tags are present, so the tags are no longer provenance only:\nwith sources:    %s\nwithout sources: %s",
				tc.tool, tc.arguments, strippedTagged, strippedPlain)
		}
	}
}
