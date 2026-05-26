package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestMCPToolsCallStats_RecentFailuresRedactsCredentialShapes pins the
// belt-and-suspenders redaction at the MCP boundary (CodeRabbit
// follow-up on PR #213). The write-side already applies the operator's
// configured secret_patterns; this read-side pass catches the cases
// where the operator has none configured or where a future store
// backend persists raw text. Defense in depth for SPEC §15.6
// ("error_message MUST NOT contain secrets").
func TestMCPToolsCallStats_RecentFailuresRedactsCredentialShapes(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Bypass the ingest-side redactor by upserting documents whose
	// error_message carries credential shapes verbatim (simulating
	// a non-SQLite store backend or an operator with no
	// secret_patterns configured).
	leaky := []model.Document{
		{RelPath: "aws.pdf", DocType: "pdf", MTimeUnix: 100, Status: "error",
			ErrorMessage: "401 from upstream: AKIAIOSFODNN7EXAMPLE rejected"},
		{RelPath: "bearer.pdf", DocType: "pdf", MTimeUnix: 200, Status: "error",
			ErrorMessage: "auth failed: Bearer abcdefghij1234567890ZYXWV mismatch"},
		{RelPath: "openai.pdf", DocType: "pdf", MTimeUnix: 300, Status: "error",
			ErrorMessage: "rate limit: key sk-proj-AbCdEfGhIjKlMnOpQr exceeded quota"},
	}
	for _, d := range leaky {
		if err := st.UpsertDocument(context.Background(), d); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	cfg := config.Default()
	cfg.StateDir = tmp
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	failuresRaw, ok := sc["recent_failures"].([]interface{})
	if !ok {
		t.Fatalf("recent_failures missing: %#v", sc)
	}
	for _, raw := range failuresRaw {
		row, _ := raw.(map[string]interface{})
		msg, _ := row["error_message"].(string)
		for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "abcdefghij1234567890ZYXWV", "sk-proj-AbCdEfGhIjKlMnOpQr"} {
			if strings.Contains(msg, leak) {
				t.Errorf("credential leaked through MCP boundary: %q (full=%q)", leak, msg)
			}
		}
		if !strings.Contains(msg, "[REDACTED]") {
			t.Errorf("expected [REDACTED] sentinel in sanitized message, got %q", msg)
		}
	}
}

// TestMCPToolsCallStats_RecentFailuresPresentWhenSeeded pins SPEC §15.6:
// when the SQLite store has documents in status='error', the dir2mcp_stats
// tool MUST include a recent_failures array carrying rel_path, doc_type,
// mtime_unix, and the sanitized error_message — newest first. This is the
// programmatic surface for the per-document failure visibility that the
// support bundle already exposes via list-files.json.
func TestMCPToolsCallStats_RecentFailuresPresentWhenSeeded(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedRecentFailureFixture(t, st)

	cfg := config.Default()
	cfg.StateDir = tmp
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	failuresRaw, ok := sc["recent_failures"].([]interface{})
	if !ok {
		t.Fatalf("recent_failures missing or not an array: %#v", sc["recent_failures"])
	}
	assertRecentFailuresFixtureOrder(t, failuresRaw)
}

// TestMCPToolsCallStats_RecentFailuresOmittedWhenHealthy pins the spec
// "MAY omit when no failures are recorded" branch: a clean corpus must
// not emit an empty recent_failures array, so consumers can rely on
// "field present" meaning "real failures exist".
func TestMCPToolsCallStats_RecentFailuresOmittedWhenHealthy(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: "ok.md", DocType: "md", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert ok doc: %v", err)
	}

	cfg := config.Default()
	cfg.StateDir = tmp
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	if _, present := sc["recent_failures"]; present {
		t.Errorf("recent_failures should be omitted on healthy corpus, got %#v", sc["recent_failures"])
	}
}

// seedRecentFailureFixture upserts a deterministic mix of failed docs
// (different mtimes) so we can assert ordering downstream.
func seedRecentFailureFixture(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	rows := []model.Document{
		{RelPath: "older.pdf", DocType: "pdf", MTimeUnix: 100, Status: "error", ErrorMessage: "docling failed: bad PDF"},
		{RelPath: "newer.pdf", DocType: "pdf", MTimeUnix: 200, Status: "error", ErrorMessage: "rep failure: oom"},
	}
	for _, d := range rows {
		if err := st.UpsertDocument(context.Background(), d); err != nil {
			t.Fatalf("seed UpsertDocument(%s): %v", d.RelPath, err)
		}
	}
}

func assertRecentFailuresFixtureOrder(t *testing.T, failuresRaw []interface{}) {
	t.Helper()
	if len(failuresRaw) != 2 {
		t.Fatalf("expected 2 recent_failures, got %d: %#v", len(failuresRaw), failuresRaw)
	}
	first, ok := failuresRaw[0].(map[string]interface{})
	if !ok {
		t.Fatalf("first failure not an object: %#v", failuresRaw[0])
	}
	if first["rel_path"] != "newer.pdf" {
		t.Errorf("newest-first order broken: got rel_path=%v", first["rel_path"])
	}
	for _, key := range []string{"rel_path", "doc_type", "mtime_unix", "error_message"} {
		if _, present := first[key]; !present {
			t.Errorf("missing required key %q in failure item: %#v", key, first)
		}
	}
	if msg, _ := first["error_message"].(string); msg == "" {
		t.Errorf("error_message empty on seeded failure: %#v", first)
	}
}

func callStatsTool(t *testing.T, mcpURL string) map[string]interface{} {
	t.Helper()
	sessionID := initializeSession(t, mcpURL)
	resp := postRPC(t, mcpURL, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stats tool HTTP %d: %s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatal("stats tool returned isError=true")
	}
	return envelope.Result.StructuredContent
}
