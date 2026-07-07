package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// flatRoutingExtractor is a model.DocumentExtractor that emits flat text only
// (like Mistral OCR) — it does NOT implement structuredExtractor, so
// extractorCanReadExt treats it as the flat path.
type flatRoutingExtractor struct{}

func (flatRoutingExtractor) Extract(context.Context, string, []byte) (string, error) {
	return "", nil
}

// structuredRoutingExtractor additionally implements structuredExtractor, so
// extractorCanReadExt treats it as the docling family.
type structuredRoutingExtractor struct{ flatRoutingExtractor }

func (structuredRoutingExtractor) ExtractStructured(context.Context, string, []byte) (StructuredExtraction, error) {
	return StructuredExtraction{}, nil
}

// TestExtractorSupportsExt pins the #394 capability split: the flat OCR path
// reads exactly the ocrMIMEType allowlist, while docling additionally reads
// OpenXML Office and tiff/bmp but not the OpenDocument/RTF/legacy-.doc family or
// gif/svg.
func TestExtractorSupportsExt(t *testing.T) {
	cases := []struct {
		ext        string
		flat       bool // expected support on the flat OCR path
		structured bool // expected support on the docling family
	}{
		// Read by both engines.
		{".pdf", true, true},
		{".png", true, true},
		{".jpg", true, true},
		{".jpeg", true, true},
		{".webp", true, true},
		// docling-only: OpenXML Office + tiff/bmp raster images.
		{".docx", false, true},
		{".pptx", false, true},
		{".xlsx", false, true},
		{".tif", false, true},
		{".tiff", false, true},
		{".bmp", false, true},
		// Read by neither engine (#394 defects 2 & 3; content support in #393).
		{".odt", false, false},
		{".odp", false, false},
		{".ods", false, false},
		{".rtf", false, false},
		{".doc", false, false},
		{".gif", false, false},
		{".svg", false, false},
	}
	for _, tc := range cases {
		if got := extractorSupportsExt(false, tc.ext); got != tc.flat {
			t.Errorf("extractorSupportsExt(flat, %q) = %v, want %v", tc.ext, got, tc.flat)
		}
		if got := extractorSupportsExt(true, tc.ext); got != tc.structured {
			t.Errorf("extractorSupportsExt(structured, %q) = %v, want %v", tc.ext, got, tc.structured)
		}
	}
}

// TestExtractorCanReadExt confirms the doc-level dispatch honors the active
// extractor kind: a docling-family extractor can read a docx an OCR-only
// extractor cannot, while both reject a .odt.
func TestExtractorCanReadExt(t *testing.T) {
	flat := &Service{extractor: flatRoutingExtractor{}}
	structured := &Service{extractor: structuredRoutingExtractor{}}
	none := &Service{}

	if flat.extractorCanReadExt("report.docx") {
		t.Error("flat OCR extractor must not claim to read .docx")
	}
	if !structured.extractorCanReadExt("report.docx") {
		t.Error("docling-family extractor must read .docx")
	}
	if structured.extractorCanReadExt("notes.odt") {
		t.Error("docling-family extractor must not claim to read .odt")
	}
	if flat.extractorCanReadExt("scan.pdf") != true {
		t.Error("both extractor kinds must read .pdf")
	}
	if none.extractorCanReadExt("scan.pdf") {
		t.Error("a nil extractor can read nothing")
	}
}

// TestExtractorLabel checks the diagnostic label reflects the structured/flat
// split used for routing.
func TestExtractorLabel(t *testing.T) {
	if got := (&Service{extractor: structuredRoutingExtractor{}}).extractorLabel(); got != "docling" {
		t.Errorf("structured extractorLabel = %q, want docling", got)
	}
	if got := (&Service{extractor: flatRoutingExtractor{}}).extractorLabel(); got == "docling" {
		t.Errorf("flat extractorLabel = %q, want a non-docling label", got)
	}
}

// TestUnsupportedExtractionErr checks the diagnostic names the offending
// extension and the active extractor so the failure is actionable.
func TestUnsupportedExtractionErr(t *testing.T) {
	err := unsupportedExtractionErr("dir/deck.odp", "docling")
	if err == nil {
		t.Fatal("expected a non-nil diagnostic")
	}
	msg := err.Error()
	for _, want := range []string{"unsupported format", ".odp", "docling"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic %q missing %q", msg, want)
		}
	}
	// The classification code stays the generic §14.4 EXTRACT_FAILED (no new
	// spec-level code introduced).
	if got := manifestErrorCode(err); got != manifestErrExtractFailed {
		t.Errorf("manifestErrorCode = %q, want %q", got, manifestErrExtractFailed)
	}
}

var _ model.DocumentExtractor = flatRoutingExtractor{}
var _ model.DocumentExtractor = structuredRoutingExtractor{}
