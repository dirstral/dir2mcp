package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// asciiPDF1100 is a small, valid PDF with uncompressed ASCII streams and no
// binary comment line. Its bytes look like text, so the MCP binary-content
// guard (looksLikeBinaryContent) does not stop it. This is the file shape that
// leaked as "page 1" text in issue #1100.
const asciiPDF1100 = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>
endobj
4 0 obj
<< /Length 44 >>
stream
BT /F1 12 Tf 72 712 Td (Hello skipped) Tj ET
endstream
endobj
trailer
<< /Root 1 0 R >>
%%EOF
`

// openFile1100Env is one MCP server over a real retrieval service and a real
// SQLite store, with a corpus root the test writes PDFs into.
type openFile1100Env struct {
	root  string
	state string
	store *store.SQLiteStore
	svc   *retrieval.Service
	url   string
}

// newOpenFile1100Env boots the production open_file chain (MCP handler ->
// retrieval.Service -> SQLite store) over an empty corpus. No PDF extractor is
// configured, so no OCR identity is folded into the cache key.
func newOpenFile1100Env(t *testing.T) *openFile1100Env {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	st := store.NewSQLiteStore(filepath.Join(state, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := retrieval.NewService(st, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(state)

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = state
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	srv := httptest.NewServer(mcp.NewServer(cfg, svc, mcp.WithStore(st)).Handler())
	t.Cleanup(srv.Close)

	return &openFile1100Env{root: root, state: state, store: st, svc: svc, url: srv.URL + cfg.MCPPath}
}

// writeCorpusFile writes body at relPath under the corpus root.
func (e *openFile1100Env) writeCorpusFile(t *testing.T, relPath, body string) {
	t.Helper()
	abs := filepath.Join(e.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relPath, err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// seedDocument upserts the store row the scan would write for doc.RelPath.
func (e *openFile1100Env) seedDocument(t *testing.T, doc model.Document) {
	t.Helper()
	if err := e.store.UpsertDocument(context.Background(), doc); err != nil {
		t.Fatalf("seed %s: %v", doc.RelPath, err)
	}
}

// seedOCRCache writes the OCR markdown ingest would cache for body. With no
// extractor identity the cache key is the sha256 of the source bytes.
func (e *openFile1100Env) seedOCRCache(t *testing.T, body, markdown string) {
	t.Helper()
	dir := filepath.Join(e.state, "cache", "ocr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".md"), []byte(markdown), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}
}

// openFile1100Result is the decoded tools/call answer plus the raw body, so a
// test can assert that no PDF syntax appears anywhere in the response.
type openFile1100Result struct {
	raw       string
	isError   bool
	code      string
	retryable bool
	content   string
}

// callOpenFile1100 calls dir2mcp_open_file with args and decodes the answer.
func callOpenFile1100(t *testing.T, url string, args map[string]interface{}) openFile1100Result {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	sessionID := initializeSession(t, url)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1100,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":%s}}`, argsJSON)
	resp := postRPC(t, url, sessionID, body)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, raw)
	}
	out := openFile1100Result{raw: string(raw), isError: envelope.Result.IsError}
	sc := envelope.Result.StructuredContent
	if errObj, ok := sc["error"].(map[string]interface{}); ok {
		out.code, _ = errObj["code"].(string)
		out.retryable, _ = errObj["retryable"].(bool)
	}
	out.content, _ = sc["content"].(string)
	return out
}

// assertNoPDFSyntax fails when the response carries any PDF source syntax.
func assertNoPDFSyntax(t *testing.T, res openFile1100Result) {
	t.Helper()
	for _, marker := range []string{"%PDF-", "endobj"} {
		if strings.Contains(res.raw, marker) {
			t.Fatalf("open_file response leaked PDF source (%q): %s", marker, res.raw)
		}
	}
}

// TestOpenFile_SkippedASCIIPDF_NeverReturnsFileBytes pins issue #1100. A PDF
// the scan skipped (no extractor, skip_reason=unsupported_format) has no text
// representation. open_file must answer DOC_TYPE_UNSUPPORTED for every span
// kind and must never return the file bytes as text (SPEC §15.4).
func TestOpenFile_SkippedASCIIPDF_NeverReturnsFileBytes(t *testing.T) {
	env := newOpenFile1100Env(t)
	const relPath = "skipped.pdf"
	env.writeCorpusFile(t, relPath, asciiPDF1100)
	env.seedDocument(t, model.Document{
		RelPath: relPath, DocType: "pdf", SourceType: "file",
		SizeBytes: int64(len(asciiPDF1100)), Status: "skipped",
		SkipReason: model.SkipReasonUnsupportedFormat,
	})

	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{name: "page=1", args: map[string]interface{}{"rel_path": relPath, "page": 1}},
		{name: "no span", args: map[string]interface{}{"rel_path": relPath}},
		{name: "line span", args: map[string]interface{}{"rel_path": relPath, "start_line": 1, "end_line": 5}},
		{name: "time span", args: map[string]interface{}{"rel_path": relPath, "start_ms": 0, "end_ms": 1000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callOpenFile1100(t, env.url, tc.args)
			assertNoPDFSyntax(t, res)
			if !res.isError {
				t.Fatalf("want an error, got success with content %q", res.content)
			}
			if res.code != "DOC_TYPE_UNSUPPORTED" {
				t.Fatalf("want DOC_TYPE_UNSUPPORTED, got %q (body=%s)", res.code, res.raw)
			}
			if res.retryable {
				t.Fatalf("DOC_TYPE_UNSUPPORTED for a skipped PDF must be non-retryable")
			}
		})
	}
}

// TestOpenFile_PendingASCIIPDF_NeverReturnsFileBytes covers a PDF whose
// extraction has not run yet (the store row is not skipped and no OCR cache
// exists). A page span with no page text answers DOC_TYPE_UNSUPPORTED (SPEC
// §15.4), the full-document and line reads answer the retryable OCR_NOT_READY,
// and no answer carries the file bytes.
func TestOpenFile_PendingASCIIPDF_NeverReturnsFileBytes(t *testing.T) {
	env := newOpenFile1100Env(t)
	const relPath = "pending.pdf"
	env.writeCorpusFile(t, relPath, asciiPDF1100)
	env.seedDocument(t, model.Document{
		RelPath: relPath, DocType: "pdf", SourceType: "file",
		SizeBytes: int64(len(asciiPDF1100)), Status: "ok",
	})

	cases := []struct {
		name          string
		args          map[string]interface{}
		wantCode      string
		wantRetryable bool
	}{
		{name: "page=1", args: map[string]interface{}{"rel_path": relPath, "page": 1}, wantCode: "DOC_TYPE_UNSUPPORTED"},
		{name: "no span", args: map[string]interface{}{"rel_path": relPath}, wantCode: "OCR_NOT_READY", wantRetryable: true},
		{name: "line span", args: map[string]interface{}{"rel_path": relPath, "start_line": 1, "end_line": 5}, wantCode: "OCR_NOT_READY", wantRetryable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callOpenFile1100(t, env.url, tc.args)
			assertNoPDFSyntax(t, res)
			if !res.isError {
				t.Fatalf("want an error, got success with content %q", res.content)
			}
			if res.code != tc.wantCode {
				t.Fatalf("want %s, got %q (body=%s)", tc.wantCode, res.code, res.raw)
			}
			if res.retryable != tc.wantRetryable {
				t.Fatalf("retryable=%v, want %v", res.retryable, tc.wantRetryable)
			}
		})
	}
}

// TestOpenFile_ExtractedPDF_StillReturnsPageText guards the success path the
// #1100 fix must keep: a PDF with stored page text answers page=N with that
// text, and with no span it answers the cached OCR markdown.
func TestOpenFile_ExtractedPDF_StillReturnsPageText(t *testing.T) {
	env := newOpenFile1100Env(t)
	const relPath = "extracted.pdf"
	env.writeCorpusFile(t, relPath, asciiPDF1100)
	env.seedDocument(t, model.Document{
		RelPath: relPath, DocType: "pdf", SourceType: "file",
		SizeBytes: int64(len(asciiPDF1100)), Status: "ok",
	})
	env.svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: relPath,
		Snippet: "Page one says hello.",
		Span:    model.Span{Kind: "page", Page: 1},
	})
	env.seedOCRCache(t, asciiPDF1100, "# Extracted\n\nPage one says hello.")

	page := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": relPath, "page": 1})
	assertNoPDFSyntax(t, page)
	if page.isError {
		t.Fatalf("page=1 on an extracted PDF: want success, got %s", page.raw)
	}
	if page.content != "Page one says hello." {
		t.Fatalf("page=1 content = %q, want the stored page text", page.content)
	}

	doc := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": relPath})
	assertNoPDFSyntax(t, doc)
	if doc.isError {
		t.Fatalf("no-span read on an extracted PDF: want success, got %s", doc.raw)
	}
	if !strings.Contains(doc.content, "# Extracted") {
		t.Fatalf("no-span content = %q, want the cached OCR markdown", doc.content)
	}
}

// TestOpenFile_TextFiles_StillReturnFileBytes guards the text-native path:
// markdown and plain text keep returning their file bytes for a full read, a
// line span and a form-feed page span.
func TestOpenFile_TextFiles_StillReturnFileBytes(t *testing.T) {
	env := newOpenFile1100Env(t)
	env.writeCorpusFile(t, "notes.md", "# Notes\nline two\nline three\n")
	env.writeCorpusFile(t, "paged.txt", "first page\fsecond page")

	full := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "notes.md"})
	if full.isError || full.content != "# Notes\nline two\nline three\n" {
		t.Fatalf("notes.md full read: got %s", full.raw)
	}
	lines := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "notes.md", "start_line": 2, "end_line": 2})
	if lines.isError || !strings.Contains(lines.content, "line two") {
		t.Fatalf("notes.md line span: got %s", lines.raw)
	}
	page := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "paged.txt", "page": 2})
	if page.isError || page.content != "second page" {
		t.Fatalf("paged.txt page=2: got %s", page.raw)
	}
}
