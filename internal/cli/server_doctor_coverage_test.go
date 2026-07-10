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
	flatExts, flatDocs := uncoveredExtractableExtensions(corpus, "auto", false, true, false)
	if want := []string{".docx", ".odt", ".tiff"}; !reflect.DeepEqual(flatExts, want) {
		t.Errorf("flat uncovered exts = %v, want %v (sorted, distinct, no empty ext)", flatExts, want)
	}
	if flatDocs != 6 { // 3 docx + 1 tiff + 2 odt
		t.Errorf("flat uncovered doc count = %d, want 6", flatDocs)
	}

	// Structured (docling): reads docx/tiff too; only the OpenDocument .odt
	// remains uncovered.
	structExts, structDocs := uncoveredExtractableExtensions(corpus, "auto", true, false, false)
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
	if exts, docs := uncoveredExtractableExtensions(corpus, "auto", false, true, false); len(exts) != 0 || docs != 0 {
		t.Errorf("fully-covered corpus reported uncovered exts=%v docs=%d, want none", exts, docs)
	}
}

// TestUncoveredExtractableExtensions_PandocEngine confirms the engine-aware
// coverage verdict folds in the pandoc (T2, #393) tier: under `auto` with docling
// AND pandoc active, the born-digital .odt/.rtf docling cannot read are COVERED by
// pandoc, while a pandoc-only corpus still reports .pdf uncovered (pandoc reads no
// PDF/raster).
func TestUncoveredExtractableExtensions_PandocEngine(t *testing.T) {
	corpus := map[string]int64{".pdf": 2, ".docx": 1, ".odt": 3, ".rtf": 1, ".doc": 1}

	// docling + pandoc active: pdf/docx via docling, odt/rtf via pandoc; only the
	// legacy binary .doc (neither engine reads) remains uncovered.
	exts, docs := uncoveredExtractableExtensions(corpus, "auto", true /*structured*/, false, true /*pandoc*/)
	if want := []string{".doc"}; !reflect.DeepEqual(exts, want) {
		t.Errorf("docling+pandoc uncovered = %v, want %v", exts, want)
	}
	if docs != 1 {
		t.Errorf("docling+pandoc uncovered docs = %d, want 1", docs)
	}

	// pandoc-only (no docling/OCR): odt/rtf/docx covered by pandoc; pdf and legacy
	// .doc are uncovered (pandoc reads neither).
	exts2, docs2 := uncoveredExtractableExtensions(corpus, "auto", false, false, true /*pandoc*/)
	if want := []string{".doc", ".pdf"}; !reflect.DeepEqual(exts2, want) {
		t.Errorf("pandoc-only uncovered = %v, want %v", exts2, want)
	}
	if docs2 != 3 { // 2 pdf + 1 doc
		t.Errorf("pandoc-only uncovered docs = %d, want 3", docs2)
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
// names only the engines that would actually help — docling for its OOXML/raster
// formats, pandoc (#393) for the born-digital family — never an already-active
// engine, and falls back to pre-conversion when no installable engine reads them.
func TestUncoveredExtractionRemedy_TailoredToEngine(t *testing.T) {
	// Flat path (no docling, no pandoc): .docx is docling-coverable and .odt is
	// pandoc-coverable → recommend both, never the stale "future extractor" wording.
	got := uncoveredExtractionRemedy([]string{".docx", ".odt"}, false, false)
	if !strings.Contains(got, "docling") || !strings.Contains(got, "pandoc") {
		t.Errorf("flat remedy %q should recommend installing docling and pandoc", got)
	}
	if strings.Contains(got, "future") {
		t.Errorf("remedy must not call pandoc a 'future' extractor now that #393 ships: %q", got)
	}
	// Neither-engine formats (no installable extractor reads them) → pre-convert,
	// no docling/pandoc suggestion.
	if got := uncoveredExtractionRemedy([]string{".gif", ".svg"}, false, false); strings.Contains(got, "docling") || strings.Contains(got, "pandoc") {
		t.Errorf("neither-engine remedy %q should not recommend an extractor", got)
	}
	// docling active, pandoc not: an uncovered .odt is pandoc-coverable → suggest
	// pandoc, and do NOT re-suggest the already-active docling.
	if got := uncoveredExtractionRemedy([]string{".odt"}, true, false); !strings.Contains(got, "pandoc") || strings.Contains(got, "install docling") {
		t.Errorf("docling-active remedy %q should recommend pandoc and not re-suggest docling", got)
	}
	// Both engines active: nothing installable helps → pre-convert only.
	if got := uncoveredExtractionRemedy([]string{".odp", ".gif"}, true, true); strings.Contains(got, "install") {
		t.Errorf("both-active remedy %q should only advise pre-conversion", got)
	}
}
