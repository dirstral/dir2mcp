package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

// TestDoclingAdapter_ServeAndLocalParity pins that the two structured-extraction
// adapters — the local CLI path (docling package: Parse -> Linearize ->
// RenderMarkdown, which the CLI extractor runs on `--to json` output) and the
// docling-serve HTTP path — produce the IDENTICAL structured contract for the
// same DoclingDocument. Without this, the two install tracks (lean docling-serve
// vs full local docling, the #145 brew tracks) could silently diverge. Runs in
// CI with no docling binary and no container.
func TestDoclingAdapter_ServeAndLocalParity(t *testing.T) {
	fixture := loadDoclingSample(t)

	// Local CLI path: the docling package is exactly what NewDoclingExtractor
	// runs on the command's JSON output.
	doc, err := docling.Parse(fixture)
	if err != nil {
		t.Fatalf("local Parse: %v", err)
	}
	localBlocks := doc.Linearize()
	localTitle := doc.Title()
	localMarkdown := docling.RenderMarkdown(localBlocks)

	// docling-serve path: same fixture returned as document.json_content.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","document":{"json_content":`+string(fixture)+`}}`)
	}))
	defer srv.Close()

	res, err := ingest.NewDoclingServeExtractor(srv.URL).
		ExtractStructured(context.Background(), "report.pdf", fixture)
	if err != nil {
		t.Fatalf("serve ExtractStructured: %v", err)
	}

	if res.Title != localTitle {
		t.Errorf("title diverges: serve=%q local=%q", res.Title, localTitle)
	}
	if res.Markdown != localMarkdown {
		t.Errorf("markdown diverges between serve and local:\n--- serve ---\n%s\n--- local ---\n%s", res.Markdown, localMarkdown)
	}
	if !reflect.DeepEqual(res.Blocks, localBlocks) {
		t.Errorf("blocks diverge between serve and local:\nserve=%+v\nlocal=%+v", res.Blocks, localBlocks)
	}
}
