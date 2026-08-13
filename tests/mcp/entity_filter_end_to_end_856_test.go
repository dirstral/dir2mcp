package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// The recognition entity/event filter END TO END (issue #856, dirstral-spec
// design 0004 §7): a real SQLite store (spans + FTS), a real vector index, the
// real retrieval service, and the two tools that advertise the parameters.
//
// tests/mcp/search_entity_filter_test.go pins that the arguments reach the
// query. It uses a retriever stub, so it cannot see whether anything applies
// them. This file closes that gap on the production chain, where #856 lived:
// the lexical candidates were fused back in unfiltered, so a filter value that
// matches nothing still returned the whole BM25 pool.

const filterEndToEndVectorDim = 2

// staticEmbedder returns one fixed query vector, so ranking is constant and a
// missing hit can only be the filter's work.
type staticEmbedder struct{}

func (staticEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for range texts {
		out = append(out, []float32{1, 0})
	}
	return out, nil
}

// annotationCorpus seeds one video document with two recognition annotations
// (one home run, one pitch) plus a text note that mentions "home run" in prose
// but carries no attribution. The note is the candidate that must NOT survive an
// event filter, and BM25 ranks it highly for the query text.
func annotationCorpus(t *testing.T, st *store.SQLiteStore) map[string]uint64 {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]uint64, 3)

	seed := func(relPath, docType, repType, text string, span model.Span) uint64 {
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
		chunkID, err := st.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: text,
			IndexKind: "text", EmbeddingStatus: "ok",
		}, []model.Span{span})
		if err != nil {
			t.Fatalf("InsertChunkWithSpans(%s): %v", relPath, err)
		}
		return uint64(chunkID)
	}

	ids["home_run"] = seed("game.mp4", "video", "recognition",
		"Heliot Ramos hits a home run to left field",
		model.Span{
			Kind: "time", StartMS: 3346398, EndMS: 3354398,
			Entities: []string{"player:heliot-ramos", "team:san-francisco-giants"},
			Event:    "home_run",
		})
	ids["pitch"] = seed("game2.mp4", "video", "recognition",
		"Logan Webb throws a pitch to the plate",
		model.Span{
			Kind: "time", StartMS: 120000, EndMS: 128000,
			Entities: []string{"player:logan-webb", "team:san-francisco-giants"},
			Event:    "pitch",
		})
	ids["note"] = seed("notes.md", "md", "raw_text",
		"match report: the home run in the seventh decided the game",
		model.Span{Kind: "lines", StartLine: 1, EndLine: 2})
	return ids
}

// entityFilterServer wires the production chain: the store serves BM25 and the
// warm-load metadata, the vector index serves dense candidates, and the MCP
// server serves both tools over HTTP.
func entityFilterServer(t *testing.T) (*httptest.Server, config.Config, map[string]uint64) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ids := annotationCorpus(t, st)

	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(st, idx, staticEmbedder{}, nil)

	// The daemon's warm-load path: read each embedded chunk's metadata back out
	// of the store and register it, then index its vector. Attribution therefore
	// reaches the filter exactly the way it does after a restart.
	tasks, err := st.ListEmbeddedChunkMetadata(ctx, "text", 100, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata: %v", err)
	}
	if len(tasks) != len(ids) {
		t.Fatalf("warm load returned %d chunks, want %d", len(tasks), len(ids))
	}
	for i, task := range tasks {
		meta := task.Metadata
		vec := make([]float32, filterEndToEndVectorDim)
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

	cfg := config.Default()
	cfg.StateDir = tmp
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	srv := httptest.NewServer(mcp.NewServer(cfg, svc, mcp.WithStore(st)).Handler())
	t.Cleanup(srv.Close)
	return srv, cfg, ids
}

// callToolHitIDs calls one tool and returns the chunk ids of its hits, sorted.
func callToolHitIDs(t *testing.T, srv *httptest.Server, cfg config.Config, toolName, arguments string) []uint64 {
	t.Helper()
	sessionID := initializeSession(t, srv.URL+cfg.MCPPath)
	resp := postRPC(t, srv.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+toolName+`","arguments":`+arguments+`}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", toolName, resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Hits []struct {
					ChunkID uint64 `json:"chunk_id"`
				} `json:"hits"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("%s: decode: %v body=%s", toolName, err, body)
	}
	if envelope.Result.IsError {
		t.Fatalf("%s returned an error result: %s", toolName, body)
	}
	out := make([]uint64, 0, len(envelope.Result.StructuredContent.Hits))
	for _, hit := range envelope.Result.StructuredContent.Hits {
		out = append(out, hit.ChunkID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestBothToolsReturnNoHitsForAnImpossibleEvent is the #856 reproduction on the
// wire: the query text matches every chunk lexically, and the filter admits
// none of them.
func TestBothToolsReturnNoHitsForAnImpossibleEvent(t *testing.T) {
	srv, cfg, _ := entityFilterServer(t)
	cases := []struct {
		tool string
		args string
	}{
		{"dir2mcp_search", `{"query":"home run","events":["no_such_event_xyz"],"k":20}`},
		{"dir2mcp_search", `{"query":"home run","entities":["player:nobody-at-all"],"k":20}`},
		{"dir2mcp_ask", `{"question":"who hit home runs?","events":["no_such_event_xyz"],"k":20}`},
		{"dir2mcp_ask", `{"question":"who hit home runs?","entities":["player:nobody-at-all"],"k":20}`},
	}
	for _, tc := range cases {
		if got := callToolHitIDs(t, srv, cfg, tc.tool, tc.args); len(got) != 0 {
			t.Fatalf("%s %s returned hits %v; a value that matches nothing must return none", tc.tool, tc.args, got)
		}
	}
}

// TestBothToolsSelectOnlyTheMatchingAnnotation pins the other direction: the
// event selects its own annotation and nothing else, even though the prose note
// is the better lexical match for the query text.
func TestBothToolsSelectOnlyTheMatchingAnnotation(t *testing.T) {
	srv, cfg, ids := entityFilterServer(t)
	want := []uint64{ids["home_run"]}
	for _, tc := range []struct{ tool, args string }{
		{"dir2mcp_search", `{"query":"home run","events":["home_run"],"k":20}`},
		{"dir2mcp_ask", `{"question":"who hit home runs?","events":["home_run"],"k":20}`},
	} {
		got := callToolHitIDs(t, srv, cfg, tc.tool, tc.args)
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("%s %s = %v, want %v (the annotation only)", tc.tool, tc.args, got, want)
		}
	}
}

// TestAnUnfilteredCallStillReturnsEveryChunk is the compatibility guard: the fix
// removes candidates only when a filter asks for it.
func TestAnUnfilteredCallStillReturnsEveryChunk(t *testing.T) {
	srv, cfg, ids := entityFilterServer(t)
	got := callToolHitIDs(t, srv, cfg, "dir2mcp_search", `{"query":"home run","k":20}`)
	if len(got) != len(ids) {
		t.Fatalf("unfiltered search returned %v, want all %d chunks", got, len(ids))
	}
}
