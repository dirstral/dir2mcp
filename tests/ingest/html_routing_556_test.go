package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
	"github.com/dirstral/dir2mcp/internal/store"
)

// fakeStructuredExtractor is a model.DocumentExtractor that also implements the
// structured (docling-family) path, so the ingest service treats it as a
// structured extractor. It returns a canned StructuredExtraction so an html
// document can be exercised through the #556 structured route without a real
// docling binary.
type fakeStructuredExtractor struct {
	res ingest.StructuredExtraction
	err error
}

func (f *fakeStructuredExtractor) Extract(context.Context, string, []byte) (string, error) {
	return f.res.Markdown, f.err
}

func (f *fakeStructuredExtractor) ExtractStructured(context.Context, string, []byte) (ingest.StructuredExtraction, error) {
	return f.res, f.err
}

// repTypesFor lists the representation types persisted for relPath.
func repTypesFor(t *testing.T, st *store.SQLiteStore, relPath string) map[string]bool {
	t.Helper()
	types, err := st.RepresentationTypesByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("RepresentationTypesByPath(%s): %v", relPath, err)
	}
	set := make(map[string]bool, len(types))
	for _, ty := range types {
		set[ty] = true
	}
	return set
}

func ingestOne(t *testing.T, svc *ingest.Service, st *store.SQLiteStore, name, body string) map[string]bool {
	t.Helper()
	f := ingest.DiscoveredFile{RelPath: name, SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", name, err)
	}
	return repTypesFor(t, st, name)
}

const sampleHTML = "<html><head><title>Page Title</title></head><body><h1>Heading</h1><p>Body text.</p></body></html>"

func newHTMLService(t *testing.T, st *store.SQLiteStore, root string) *ingest.Service {
	t.Helper()
	return mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
}

// TestHTML_RoutesToStructured_WhenDoclingActive is the #556 core: when a
// structured (docling family) extractor is active, an .html document is routed
// through it, producing an extracted_markdown representation (structure preserved)
// instead of flat raw_text (§7.4.A).
func TestHTML_RoutesToStructured_WhenDoclingActive(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "page.html"), sampleHTML)
	st := newRealStore(t)
	svc := newHTMLService(t, st, root)
	svc.SetDocumentExtractor(&fakeStructuredExtractor{res: ingest.StructuredExtraction{
		Markdown: "# Heading\n\nBody text.",
		Title:    "Page Title",
		Blocks: []docling.Block{
			{Label: "section_header", Level: 1, Section: nil, Text: "Heading"},
			{Label: "paragraph", Section: []string{"Heading"}, Text: "Body text."},
		},
	}})

	reps := ingestOne(t, svc, st, "page.html", sampleHTML)
	if !reps[ingest.RepTypeExtractedMarkdown] {
		t.Errorf("html with active docling did not produce extracted_markdown; reps=%v", reps)
	}
	if reps[ingest.RepTypeRawText] {
		t.Errorf("html with active docling must not also produce raw_text (dual-path is one or the other); reps=%v", reps)
	}
	if doc := documentByPath(t, st, "page.html"); doc.Status != "ok" {
		t.Errorf("html status = %q, want ok", doc.Status)
	}
}

// TestHTML_FallsBackToRawText_WhenFlatExtractor pins the §7.4.A markup boundary:
// with only a flat OCR extractor active (which cannot read html), html falls back
// to the raw_text baseline — never dropped, never mis-routed to the flat engine.
func TestHTML_FallsBackToRawText_WhenFlatExtractor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "page.html"), sampleHTML)
	st := newRealStore(t)
	svc := newHTMLService(t, st, root)
	svc.SetDocumentExtractor(&fakeExtractor{text: "should never be used for html"})

	reps := ingestOne(t, svc, st, "page.html", sampleHTML)
	if !reps[ingest.RepTypeRawText] {
		t.Errorf("html with flat extractor did not fall back to raw_text; reps=%v", reps)
	}
	if reps[ingest.RepTypeExtractedMarkdown] {
		t.Errorf("html must not produce extracted_markdown on the flat path; reps=%v", reps)
	}
}

// TestHTML_FallsBackToRawText_WhenNoExtractor pins the no-regression guarantee:
// with no extractor at all (the lean build), html is still indexed as raw_text
// exactly as before docling routing existed.
func TestHTML_FallsBackToRawText_WhenNoExtractor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "page.html"), sampleHTML)
	st := newRealStore(t)
	svc := newHTMLService(t, st, root)
	// No SetDocumentExtractor: the lean-build path.

	reps := ingestOne(t, svc, st, "page.html", sampleHTML)
	if !reps[ingest.RepTypeRawText] {
		t.Errorf("html with no extractor did not produce raw_text; reps=%v", reps)
	}
	if reps[ingest.RepTypeExtractedMarkdown] {
		t.Errorf("html with no extractor must not produce extracted_markdown; reps=%v", reps)
	}
}

// TestHTML_FallsBackToRawText_WhenStructuredYieldsNothing guards the §7.4.A
// baseline on a degraded structured extractor: an active docling family engine
// that returns no parseable structure (or errors) must not drop the html — it
// falls back to raw_text.
func TestHTML_FallsBackToRawText_WhenStructuredYieldsNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "page.html"), sampleHTML)
	st := newRealStore(t)
	svc := newHTMLService(t, st, root)
	// Structured extractor active but yields zero blocks: baseline must apply.
	svc.SetDocumentExtractor(&fakeStructuredExtractor{res: ingest.StructuredExtraction{}})

	reps := ingestOne(t, svc, st, "page.html", sampleHTML)
	if !reps[ingest.RepTypeRawText] {
		t.Errorf("empty structured extraction did not fall back to raw_text; reps=%v", reps)
	}
	if reps[ingest.RepTypeExtractedMarkdown] {
		t.Errorf("empty structured extraction must not persist an empty extracted_markdown; reps=%v", reps)
	}
}
