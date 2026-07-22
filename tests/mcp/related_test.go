package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// relatedRetrieverStub is a model.Retriever that additionally implements
// model.RelatedSearcher, capturing the RelatedQuery the dir2mcp_related handler
// forwards and returning a canned RelatedResult (or a canned error).
type relatedRetrieverStub struct {
	lastQuery model.RelatedQuery
	called    bool
	result    model.RelatedResult
	err       error
}

func (s *relatedRetrieverStub) Search(context.Context, model.SearchQuery) ([]model.SearchHit, error) {
	return nil, model.ErrNotImplemented
}
func (s *relatedRetrieverStub) Ask(context.Context, string, model.SearchQuery) (model.AskResult, error) {
	return model.AskResult{}, model.ErrNotImplemented
}
func (s *relatedRetrieverStub) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", model.ErrNotImplemented
}
func (s *relatedRetrieverStub) Stats(context.Context) (model.Stats, error) {
	return model.Stats{}, model.ErrNotImplemented
}
func (s *relatedRetrieverStub) IndexingComplete(context.Context) (bool, error) { return true, nil }

func (s *relatedRetrieverStub) Related(_ context.Context, q model.RelatedQuery) (model.RelatedResult, error) {
	s.called = true
	s.lastQuery = q
	if s.err != nil {
		return model.RelatedResult{}, s.err
	}
	res := s.result
	res.K = q.K
	if q.SourceChunkID != 0 {
		res.SourceChunkID = q.SourceChunkID
		res.HasSourceChunkID = true
	}
	if res.SourceRelPath == "" {
		res.SourceRelPath = q.SourceRelPath
	}
	if res.IndexUsed == "" {
		res.IndexUsed = "text"
	}
	return res, nil
}

var _ model.Retriever = (*relatedRetrieverStub)(nil)
var _ model.RelatedSearcher = (*relatedRetrieverStub)(nil)

func relatedStructured(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Result struct {
			StructuredContent map[string]interface{} `json:"structuredContent"`
			IsError           bool                   `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Result.IsError {
		t.Fatalf("unexpected tool error: %s", body)
	}
	return env.Result.StructuredContent
}

func TestMCPRelated_ChunkIDForwardedAndEchoed(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{result: model.RelatedResult{
		SourceRelPath: "docs/a.txt",
		Hits:          []model.SearchHit{{ChunkID: 7, RelPath: "docs/b.txt", Score: 0.9, Snippet: "hi", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}}},
	}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"chunk_id":5,"k":3,"exclude_same_document":false}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if retriever.lastQuery.SourceChunkID != 5 {
		t.Fatalf("SourceChunkID = %d, want 5", retriever.lastQuery.SourceChunkID)
	}
	if retriever.lastQuery.SourceRelPath != "" {
		t.Fatalf("SourceRelPath = %q, want empty for a chunk_id request", retriever.lastQuery.SourceRelPath)
	}
	if retriever.lastQuery.ExcludeSameDocument {
		t.Fatal("ExcludeSameDocument = true, want false (forwarded verbatim)")
	}
	if retriever.lastQuery.K != 3 {
		t.Fatalf("K = %d, want 3", retriever.lastQuery.K)
	}
	body, _ := io.ReadAll(resp.Body)
	sc := relatedStructured(t, body)
	if got, ok := sc["source_chunk_id"]; !ok || int(got.(float64)) != 5 {
		t.Fatalf("source_chunk_id = %v, want 5", sc["source_chunk_id"])
	}
	if sc["source_rel_path"] != "docs/a.txt" {
		t.Fatalf("source_rel_path = %v, want docs/a.txt", sc["source_rel_path"])
	}
	if sc["index_used"] != "text" {
		t.Fatalf("index_used = %v, want text", sc["index_used"])
	}
}

func TestMCPRelated_RelPathOmitsSourceChunkID(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{result: model.RelatedResult{Hits: []model.SearchHit{}}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"rel_path":"docs/a.txt","k":5,"path_prefix":"docs","languages":["en"],"date_from":"2026-01-01"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if retriever.lastQuery.SourceRelPath != "docs/a.txt" {
		t.Fatalf("SourceRelPath = %q, want docs/a.txt", retriever.lastQuery.SourceRelPath)
	}
	if retriever.lastQuery.PathPrefix != "docs" || len(retriever.lastQuery.Languages) != 1 || retriever.lastQuery.Languages[0] != "en" {
		t.Fatalf("filters not forwarded: %+v", retriever.lastQuery)
	}
	if retriever.lastQuery.DateFrom == 0 {
		t.Fatal("date_from not forwarded")
	}
	body, _ := io.ReadAll(resp.Body)
	sc := relatedStructured(t, body)
	if _, ok := sc["source_chunk_id"]; ok {
		t.Fatalf("source_chunk_id must be omitted for a rel_path request, got %v", sc["source_chunk_id"])
	}
	if sc["source_rel_path"] != "docs/a.txt" {
		t.Fatalf("source_rel_path = %v, want docs/a.txt", sc["source_rel_path"])
	}
}

func TestMCPRelated_OneOfViolationBoth(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"chunk_id":1,"rel_path":"docs/a.txt"}}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("supplying both chunk_id and rel_path must be INVALID_FIELD, body=%s", got)
	}
	if retriever.called {
		t.Fatal("retriever.Related must not be called on a oneOf violation")
	}
}

func TestMCPRelated_OneOfViolationNeither(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"k":5}}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("supplying neither chunk_id nor rel_path must be INVALID_FIELD, body=%s", got)
	}
	if retriever.called {
		t.Fatal("retriever.Related must not be called on a oneOf violation")
	}
}

func TestMCPRelated_SourceNotFoundIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{err: model.ErrRelatedSourceNotFound}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"chunk_id":999}}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("an unresolvable source must be INVALID_FIELD, body=%s", got)
	}
}

func TestMCPRelated_ExcludeSameDocumentDefaultsTrue(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{result: model.RelatedResult{SourceRelPath: "docs/a.txt", Hits: []model.SearchHit{}}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"chunk_id":5}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if !retriever.lastQuery.ExcludeSameDocument {
		t.Fatal("exclude_same_document must default to true when omitted")
	}
}

func TestMCPRelated_UnknownArgumentRejected(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &relatedRetrieverStub{}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_related","arguments":{"chunk_id":1,"language_match":"strict"}}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("an undeclared argument must be INVALID_FIELD, body=%s", got)
	}
	if retriever.called {
		t.Fatal("retriever.Related must not be called when an undeclared argument is present")
	}
}
