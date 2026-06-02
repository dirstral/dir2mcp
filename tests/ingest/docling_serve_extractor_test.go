package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
)

// servedDoc is a minimal DoclingDocument the docling-serve stub returns inside
// document.json_content — the same shape the CLI emits via `--to json`.
const servedDoc = `{
  "schema_name": "DoclingDocument",
  "version": "1.2.0",
  "name": "Served Doc",
  "pages": { "1": { "size": { "width": 612, "height": 792 }, "page_no": 1 } },
  "body": { "self_ref": "#/body", "children": [
    { "$ref": "#/texts/0" },
    { "$ref": "#/texts/1" }
  ]},
  "texts": [
    { "self_ref": "#/texts/0", "label": "title", "text": "Served Doc",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 700, "r": 540, "b": 720, "coord_origin": "BOTTOMLEFT" }}] },
    { "self_ref": "#/texts/1", "label": "paragraph", "text": "Hello from docling-serve.",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 600, "r": 540, "b": 620, "coord_origin": "BOTTOMLEFT" }}] }
  ]
}`

func TestDoclingServeExtractor_Extract_ParsesJSONContent(t *testing.T) {
	var gotPath, gotForm, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		gotForm = r.FormValue("to_formats")
		if f, _, err := r.FormFile("files"); err == nil {
			b, _ := io.ReadAll(f)
			gotFile = string(b)
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","document":{"json_content":`+servedDoc+`}}`)
	}))
	defer srv.Close()

	ext := ingest.NewDoclingServeExtractor(srv.URL)
	md, err := ext.Extract(context.Background(), "report.pdf", []byte("PDF-BYTES"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(md, "Hello from docling-serve.") {
		t.Errorf("rendered markdown missing body text; got:\n%s", md)
	}

	// The request must hit the convert endpoint, request JSON output, and carry
	// the uploaded file content.
	if gotPath != "/v1/convert/file" {
		t.Errorf("convert path = %q, want /v1/convert/file", gotPath)
	}
	if gotForm != "json" {
		t.Errorf("to_formats = %q, want json", gotForm)
	}
	if gotFile != "PDF-BYTES" {
		t.Errorf("uploaded file = %q, want PDF-BYTES", gotFile)
	}
}

func TestDoclingServeExtractor_Extract_ErrorsOnNon200(t *testing.T) {
	// A neutral, non-credential-shaped sentinel so secret scanners don't flag
	// this fixture; it still stands in for an untrusted, must-not-leak body.
	const responseBodyMarker = "do-not-echo-this-response-body"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, responseBodyMarker, http.StatusInternalServerError)
	}))
	defer srv.Close()

	ext := ingest.NewDoclingServeExtractor(srv.URL)
	_, err := ext.Extract(context.Background(), "report.pdf", []byte("x"))
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	// The error is persisted as Document.ErrorMessage — it must not echo the
	// (untrusted) response body.
	if strings.Contains(err.Error(), responseBodyMarker) {
		t.Errorf("error leaked response body: %v", err)
	}
}

func TestDoclingServeExtractor_PreservesQueryWhenJoiningPath(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","document":{"json_content":`+servedDoc+`}}`)
	}))
	defer srv.Close()

	// A serve_url carrying a routing/auth query must still hit the convert path
	// (not "?token=secret/v1/convert/file") and keep the query on the wire.
	ext := ingest.NewDoclingServeExtractor(srv.URL + "/?token=secret")
	if _, err := ext.Extract(context.Background(), "report.pdf", []byte("x")); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if gotPath != "/v1/convert/file" {
		t.Errorf("convert path = %q, want /v1/convert/file", gotPath)
	}
	if gotQuery != "token=secret" {
		t.Errorf("query = %q, want token=secret preserved on the wire", gotQuery)
	}
}

func TestSanitizeServeURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:5001":                   "http://127.0.0.1:5001",
		"http://user:pass@host:5001/v1?token=sec": "http://host:5001/v1",
		"https://example.com/base/":               "https://example.com/base/",
	}
	for in, want := range cases {
		if got := ingest.SanitizeServeURL(in); got != want {
			t.Errorf("SanitizeServeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDoclingServeExtractor_Extract_ErrorsOnMissingJSONContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"failure","document":{"json_content":null}}`)
	}))
	defer srv.Close()

	ext := ingest.NewDoclingServeExtractor(srv.URL)
	if _, err := ext.Extract(context.Background(), "report.pdf", []byte("x")); err == nil {
		t.Fatal("expected error when json_content is missing, got nil")
	}
}

func TestProbeDoclingServe(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probe path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	if err := ingest.ProbeDoclingServe(context.Background(), healthy.URL); err != nil {
		t.Errorf("ProbeDoclingServe(healthy) = %v, want nil", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := ingest.ProbeDoclingServe(context.Background(), down.URL); err == nil {
		t.Error("ProbeDoclingServe(503) = nil, want error")
	}

	if err := ingest.ProbeDoclingServe(context.Background(), ""); err == nil {
		t.Error("ProbeDoclingServe(\"\") = nil, want error")
	}
}
