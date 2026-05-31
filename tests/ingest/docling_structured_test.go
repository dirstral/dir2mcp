package tests

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
)

// structuredJSON is a minimal DoclingDocument with a title, a section header,
// and a paragraph carrying provenance, used to exercise the structured path
// through the docling extractor without a real docling binary.
const structuredJSON = `{
  "schema_name": "DoclingDocument", "version": "1.2.0", "name": "Report",
  "pages": { "1": { "size": { "width": 612, "height": 792 }, "page_no": 1 } },
  "body": { "self_ref": "#/body", "children": [
    { "$ref": "#/texts/0" }, { "$ref": "#/texts/1" }, { "$ref": "#/texts/2" }
  ]},
  "texts": [
    { "self_ref": "#/texts/0", "label": "title", "text": "Report" },
    { "self_ref": "#/texts/1", "label": "section_header", "level": 1, "text": "Results" },
    { "self_ref": "#/texts/2", "label": "paragraph", "text": "Revenue grew.",
      "prov": [{ "page_no": 1, "bbox": { "l": 72, "t": 100, "r": 500, "b": 120, "coord_origin": "TOPLEFT" }}] }
  ]
}`

// TestDoclingExtractor_ExtractStructured pins the structured path: the command
// emits DoclingDocument JSON; the extractor returns ordered blocks, rendered
// Markdown, and the title.
func TestDoclingExtractor_ExtractStructured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	ex := ingest.NewDoclingExtractor("cat {input}")
	res, err := ex.ExtractStructured(context.Background(), "doc.json", []byte(structuredJSON))
	if err != nil {
		t.Fatalf("ExtractStructured: %v", err)
	}
	if res.Title != "Report" {
		t.Errorf("title = %q, want Report", res.Title)
	}
	if len(res.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(res.Blocks))
	}
	for _, want := range []string{"# Report", "# Results", "Revenue grew."} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, res.Markdown)
		}
	}
	// The paragraph block must carry its page provenance.
	found := false
	for _, b := range res.Blocks {
		if strings.TrimSpace(b.Text) == "Revenue grew." {
			found = true
			if b.Page != 1 || b.BBox == nil {
				t.Errorf("paragraph provenance lost: page=%d bbox=%v", b.Page, b.BBox)
			}
			if len(b.Section) != 1 || b.Section[0] != "Results" {
				t.Errorf("paragraph section = %v, want [Results]", b.Section)
			}
		}
	}
	if !found {
		t.Error("paragraph block not found")
	}
}

// TestDoclingExtractor_Extract_RendersStructuredAsMarkdown pins that the flat
// Extract entrypoint linearizes a DoclingDocument to Markdown (so the default
// `--to json` command still yields Markdown for the extracted_markdown rep).
func TestDoclingExtractor_Extract_RendersStructuredAsMarkdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	ex := ingest.NewDoclingExtractor("cat {input}")
	md, err := ex.Extract(context.Background(), "doc.json", []byte(structuredJSON))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(md, "# Results") || !strings.Contains(md, "Revenue grew.") {
		t.Errorf("structured output not rendered to markdown:\n%s", md)
	}
	// Raw JSON must not leak into the representation text.
	if strings.Contains(md, "self_ref") || strings.Contains(md, "schema_name") {
		t.Errorf("raw docling JSON leaked into markdown:\n%s", md)
	}
}

// TestDoclingExtractor_Extract_FlatFallback pins backward compatibility: a
// custom command emitting non-JSON (flat Markdown) is returned verbatim.
func TestDoclingExtractor_Extract_FlatFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	ex := ingest.NewDoclingExtractor("cat {input}")
	md, err := ex.Extract(context.Background(), "sample.md", []byte("# Plain markdown\n\nbody"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if strings.TrimSpace(md) != "# Plain markdown\n\nbody" {
		t.Errorf("flat fallback altered output: %q", md)
	}
}
