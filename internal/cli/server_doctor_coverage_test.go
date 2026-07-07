package cli

import (
	"reflect"
	"strings"
	"testing"
)

// TestUncoveredExtractableExtensions_NamesSpecificFormats proves the coverage
// diagnostic names the exact corpus extensions the active extractor cannot read
// (#395), rather than only reporting a count. It exercises both engine kinds
// against a fixture corpus and checks the returned extensions are sorted,
// deduplicated to distinct formats, and the document tally sums the uncovered
// counts.
func TestUncoveredExtractableExtensions_NamesSpecificFormats(t *testing.T) {
	// Fixture corpus: a mix the flat OCR path cannot read (.docx/.tiff/.odt),
	// formats it can (.pdf/.png), and an extension-less asset that must be
	// ignored (no format to name).
	corpus := map[string]int64{
		".pdf":  4,
		".png":  2,
		".docx": 3,
		".tiff": 1,
		".odt":  2,
		"":      5,
	}

	// Flat OCR (mistral-ocr): reads only pdf/png here; docx/tiff/odt uncovered.
	flatExts, flatDocs := uncoveredExtractableExtensions(corpus, false)
	if want := []string{".docx", ".odt", ".tiff"}; !reflect.DeepEqual(flatExts, want) {
		t.Errorf("flat uncovered exts = %v, want %v (sorted, distinct, no empty ext)", flatExts, want)
	}
	if flatDocs != 6 { // 3 docx + 1 tiff + 2 odt
		t.Errorf("flat uncovered doc count = %d, want 6", flatDocs)
	}

	// Structured (docling): reads docx/tiff too; only the OpenDocument .odt
	// remains uncovered.
	structExts, structDocs := uncoveredExtractableExtensions(corpus, true)
	if want := []string{".odt"}; !reflect.DeepEqual(structExts, want) {
		t.Errorf("structured uncovered exts = %v, want %v", structExts, want)
	}
	if structDocs != 2 {
		t.Errorf("structured uncovered doc count = %d, want 2", structDocs)
	}
}

// TestUncoveredExtractableExtensions_FullCoverage confirms no false positive when
// every extractable format is readable by the active engine.
func TestUncoveredExtractableExtensions_FullCoverage(t *testing.T) {
	corpus := map[string]int64{".pdf": 3, ".png": 1, ".jpg": 2}
	if exts, docs := uncoveredExtractableExtensions(corpus, false); len(exts) != 0 || docs != 0 {
		t.Errorf("fully-covered corpus reported uncovered exts=%v docs=%d, want none", exts, docs)
	}
}

// TestExtractorIsStructured maps resolved extractor names to the structured/flat
// distinction the capability table uses.
func TestExtractorIsStructured(t *testing.T) {
	for name, want := range map[string]bool{
		"docling":       true,
		"docling-serve": true,
		"mistral-ocr":   false,
		"":              false,
		"unknown":       false,
	} {
		if got := extractorIsStructured(name); got != want {
			t.Errorf("extractorIsStructured(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestUncoveredExtractionRemedy_TailoredToEngine checks the remediation hint
// points a flat-OCR user at docling for the docling-coverable formats, and tells
// a docling user that docling cannot help with the neither-engine formats.
func TestUncoveredExtractionRemedy_TailoredToEngine(t *testing.T) {
	// Flat path with a docling-coverable format present → name docling.
	if got := uncoveredExtractionRemedy([]string{".docx", ".odt"}, false); !strings.Contains(got, "docling") {
		t.Errorf("flat remedy %q should recommend installing docling", got)
	}
	// Flat path where docling would not help either (neither-engine formats).
	if got := uncoveredExtractionRemedy([]string{".gif", ".svg"}, false); strings.Contains(got, "Install docling") {
		t.Errorf("flat remedy for neither-engine formats %q should not recommend docling", got)
	}
	// Structured path: docling is already active and cannot import these.
	if got := uncoveredExtractionRemedy([]string{".odt"}, true); !strings.Contains(got, "cannot import") {
		t.Errorf("structured remedy %q should say docling cannot import them", got)
	}
}
