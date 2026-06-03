package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

// loadDoclingSample reads the checked-in golden DoclingDocument fixture. It is
// the same JSON shape the docling CLI (`--to json`) and docling-serve
// (document.json_content) emit, so it pins the adapter's structural contract.
func loadDoclingSample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "docling_sample.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// findBlock returns the first block whose label and substring match.
func findBlock(blocks []docling.Block, label, contains string) (docling.Block, bool) {
	for _, b := range blocks {
		if b.Label == label && strings.Contains(b.Text, contains) {
			return b, true
		}
	}
	return docling.Block{}, false
}

func countLabel(blocks []docling.Block, label string) int {
	n := 0
	for _, b := range blocks {
		if b.Label == label {
			n++
		}
	}
	return n
}

// assertDoclingSampleContract pins the structural contract the rest of the
// pipeline (chunking, citations) depends on: reading order, section breadcrumb,
// per-element page/bbox provenance, labels, atomic tables, and figure captions.
func assertDoclingSampleContract(t *testing.T, title, markdown string, blocks []docling.Block) {
	t.Helper()

	if title != "Quarterly Report" {
		t.Errorf("title = %q, want %q", title, "Quarterly Report")
	}

	// Rendered markdown: heading levels, table as Markdown, caption, and the
	// picture annotation as searchable text.
	for _, want := range []string{
		"# Quarterly Report",
		"# Overview",
		"## Regional Details",
		"EMEA led growth this quarter.",
		"| Region | Revenue |",
		"| EMEA | 100 |",
		"Table 1: Revenue by region.",
		"Figure 1: Revenue chart.",
		"Bar chart of revenue by region.",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, markdown)
		}
	}

	// The h2 paragraph carries the full breadcrumb and page-2 provenance.
	if p, ok := findBlock(blocks, docling.LabelParagraph, "EMEA led growth"); ok {
		if got := strings.Join(p.Section, " > "); got != "Overview > Regional Details" {
			t.Errorf("paragraph breadcrumb = %q, want %q", got, "Overview > Regional Details")
		}
		if p.Page != 2 {
			t.Errorf("paragraph page = %d, want 2", p.Page)
		}
		if p.BBox == nil || p.BBox.Page != 2 {
			t.Errorf("paragraph bbox = %+v, want non-nil on page 2", p.BBox)
		}
	} else {
		t.Error("expected a paragraph block for the h2 body text")
	}

	// The table is one atomic block under the right section.
	if n := countLabel(blocks, docling.LabelTable); n != 1 {
		t.Errorf("table block count = %d, want 1 (atomic)", n)
	}
	if tbl, ok := findBlock(blocks, docling.LabelTable, "| EMEA | 100 |"); ok {
		if got := strings.Join(tbl.Section, " > "); got != "Overview > Regional Details" {
			t.Errorf("table breadcrumb = %q, want %q", got, "Overview > Regional Details")
		}
	} else {
		t.Error("expected an atomic table block with rendered rows")
	}

	// Title block is present and labeled.
	if _, ok := findBlock(blocks, docling.LabelTitle, "Quarterly Report"); !ok {
		t.Error("expected a title block")
	}
}

// TestDoclingPipeline_GoldenContract pins the parse -> linearize -> render
// contract directly against the golden DoclingDocument fixture. It runs in CI
// (no docling binary) and catches adapter-protocol regressions in the
// structured-extraction pipeline (#145).
func TestDoclingPipeline_GoldenContract(t *testing.T) {
	doc, err := docling.Parse(loadDoclingSample(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	blocks := doc.Linearize()
	markdown := docling.RenderMarkdown(blocks)
	assertDoclingSampleContract(t, doc.Title(), markdown, blocks)
}

// TestDoclingServeAdapter_GoldenContract drives the SAME golden fixture through
// the docling-serve HTTP adapter (httptest, no container) and asserts the
// adapter produces the identical structured contract. This exercises the
// "docling-enabled path for supported formats" end-to-end in CI (#145).
func TestDoclingServeAdapter_GoldenContract(t *testing.T) {
	fixture := loadDoclingSample(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if got := r.FormValue("to_formats"); got != "json" {
			t.Errorf("to_formats = %q, want json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","document":{"json_content":`+string(fixture)+`}}`)
	}))
	defer srv.Close()

	ext := ingest.NewDoclingServeExtractor(srv.URL)
	res, err := ext.ExtractStructured(context.Background(), "report.pdf", []byte("PDF-BYTES"))
	if err != nil {
		t.Fatalf("ExtractStructured: %v", err)
	}
	assertDoclingSampleContract(t, res.Title, res.Markdown, res.Blocks)
}
