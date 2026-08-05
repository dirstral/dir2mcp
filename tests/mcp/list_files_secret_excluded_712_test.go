package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #712: list_files reported every persisted `secret_excluded` document as
// `status: "ok"`. `normalizeFileStatus` recognised only `skipped` and `error`,
// so a document deliberately WITHHELD for containing secrets fell through the
// default arm and was reported as successfully indexed.
//
// That is not merely cosmetic. It tells an agent a sensitive file was indexed
// when it has zero searchable chunks, and it made the surfaces contradict each
// other: CorpusStats counts `status IN ('skipped','secret_excluded')` as
// skipped, and skip_summary.go pairs them the same way, so `stats` reported a
// skip for the very row `list_files` called healthy. It also undid the
// operator-facing audit value #425 restored by persisting the state at all.
//
// The published schema (spec/tools/schemas/list_files.json) pins status to
// `ok|skipped|error`, so `skipped` is the conforming honest projection and no
// spec change is needed.

func TestListFilesReportsASecretExcludedDocumentAsSkipped(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := []model.Document{
		{RelPath: "notes/ok.md", DocType: "md", MTimeUnix: 100, Status: "ok"},
		{RelPath: "config/.env", DocType: "txt", MTimeUnix: 200, Status: "secret_excluded"},
		{RelPath: "notes/big.bin", DocType: "bin", MTimeUnix: 300, Status: "skipped"},
		{RelPath: "notes/bad.pdf", DocType: "pdf", MTimeUnix: 400, Status: "error"},
	}
	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, seed)

	statuses := listFileStatuses(t, tmp, root, st)

	if got := statuses["config/.env"]; got != "skipped" {
		t.Fatalf("a withheld document is reported as %q; an agent is told a secret-bearing file was indexed (#712)", got)
	}
	// The projection must not disturb the states that were already correct.
	if got := statuses["notes/ok.md"]; got != "ok" {
		t.Fatalf("healthy document reported as %q", got)
	}
	if got := statuses["notes/big.bin"]; got != "skipped" {
		t.Fatalf("skipped document reported as %q", got)
	}
	if got := statuses["notes/bad.pdf"]; got != "error" {
		t.Fatalf("errored document reported as %q", got)
	}
}

// TestListFilesStatusesStayInsideThePublishedEnum guards the projection itself
// rather than one value: whatever the store grows next, this tool may only emit
// the three states its schema advertises.
func TestListFilesStatusesStayInsideThePublishedEnum(t *testing.T) {
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	docs := make([]model.Document, 0, 5)
	for i, status := range []string{"ok", "skipped", "error", "secret_excluded", "pending"} {
		docs = append(docs, model.Document{
			RelPath:   "f" + string(rune('a'+i)) + ".md",
			DocType:   "md",
			MTimeUnix: int64(100 * (i + 1)),
			Status:    status,
		})
	}
	root := filepath.Join(tmp, "corpus")
	seedCorpus(t, st, root, docs)

	allowed := map[string]bool{"ok": true, "skipped": true, "error": true}
	for relPath, status := range listFileStatuses(t, tmp, root, st) {
		if !allowed[status] {
			t.Fatalf("%s reported status %q, outside the published enum ok|skipped|error", relPath, status)
		}
	}
}

// seedCorpus writes a real file for each document and persists the row. The
// listing resolves every row against the corpus root and drops what does not
// exist there, so a store-only fixture returns nothing and would make this test
// vacuous rather than failing.
func seedCorpus(t *testing.T, st *store.SQLiteStore, root string, docs []model.Document) {
	t.Helper()
	for _, d := range docs {
		full := filepath.Join(root, filepath.FromSlash(d.RelPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", d.RelPath, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", d.RelPath, err)
		}
		if err := st.UpsertDocument(context.Background(), d); err != nil {
			t.Fatalf("seed %s: %v", d.RelPath, err)
		}
	}
}

// listFileStatuses calls the real dir2mcp_list_files tool over HTTP and returns
// rel_path -> status. Going through the tool rather than the helper is the
// point: the defect was in the projection the tool applies, and a unit test on
// the store would have shown the correct `secret_excluded` all along.
func listFileStatuses(t *testing.T, stateDir, root string, st *store.SQLiteStore) map[string]string {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":50,"offset":0}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("list_files status=%d body=%s", resp.StatusCode, payload)
	}

	var envelope struct {
		Result struct {
			StructuredContent struct {
				Files []struct {
					RelPath string `json:"rel_path"`
					Status  string `json:"status"`
				} `json:"files"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode list_files response: %v", err)
	}
	files := envelope.Result.StructuredContent.Files
	if len(files) == 0 {
		t.Fatal("list_files returned no files")
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[strings.TrimSpace(f.RelPath)] = f.Status
	}
	return out
}
