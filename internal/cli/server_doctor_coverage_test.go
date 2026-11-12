package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

var (
	flatOnly          = ingest.ActiveEngines{FlatOCR: true}
	structuredOnly    = ingest.ActiveEngines{Structured: true}
	noEngines         = ingest.ActiveEngines{}
	doclingAndPandoc  = ingest.ActiveEngines{Structured: true, Pandoc: true}
	pandocOnlyEngines = ingest.ActiveEngines{Pandoc: true}
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
	flatExts, flatDocs := uncoveredExtractableExtensions(corpus, "auto", flatOnly)
	if want := []string{".docx", ".odt", ".tiff"}; !reflect.DeepEqual(flatExts, want) {
		t.Errorf("flat uncovered exts = %v, want %v (sorted, distinct, no empty ext)", flatExts, want)
	}
	if flatDocs != 6 { // 3 docx + 1 tiff + 2 odt
		t.Errorf("flat uncovered doc count = %d, want 6", flatDocs)
	}

	// Structured (docling): reads docx/tiff too; only the OpenDocument .odt
	// remains uncovered.
	structExts, structDocs := uncoveredExtractableExtensions(corpus, "auto", structuredOnly)
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
	if exts, docs := uncoveredExtractableExtensions(corpus, "auto", flatOnly); len(exts) != 0 || docs != 0 {
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
	exts, docs := uncoveredExtractableExtensions(corpus, "auto", doclingAndPandoc)
	if want := []string{".doc"}; !reflect.DeepEqual(exts, want) {
		t.Errorf("docling+pandoc uncovered = %v, want %v", exts, want)
	}
	if docs != 1 {
		t.Errorf("docling+pandoc uncovered docs = %d, want 1", docs)
	}

	// pandoc-only (no docling/OCR): odt/rtf/docx covered by pandoc; pdf and legacy
	// .doc are uncovered (pandoc reads neither).
	exts2, docs2 := uncoveredExtractableExtensions(corpus, "auto", pandocOnlyEngines)
	if want := []string{".doc", ".pdf"}; !reflect.DeepEqual(exts2, want) {
		t.Errorf("pandoc-only uncovered = %v, want %v", exts2, want)
	}
	if docs2 != 3 { // 2 pdf + 1 doc
		t.Errorf("pandoc-only uncovered docs = %d, want 3", docs2)
	}

	// docling + pandoc + libreoffice (spec 0.93.0): the legacy .doc is covered
	// too, so nothing remains uncovered.
	all := ingest.ActiveEngines{Structured: true, Pandoc: true, LibreOffice: true}
	if exts3, docs3 := uncoveredExtractableExtensions(corpus, "auto", all); len(exts3) != 0 || docs3 != 0 {
		t.Errorf("docling+pandoc+libreoffice uncovered = %v (%d docs), want none", exts3, docs3)
	}
	// The libreoffice pin covers only .doc.
	exts4, _ := uncoveredExtractableExtensions(corpus, "libreoffice", all)
	if want := []string{".docx", ".odt", ".pdf", ".rtf"}; !reflect.DeepEqual(exts4, want) {
		t.Errorf("libreoffice pin uncovered = %v, want %v", exts4, want)
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
	got := uncoveredExtractionRemedy([]string{".docx", ".odt"}, noEngines, "")
	if !strings.Contains(got, "docling") || !strings.Contains(got, "pandoc") {
		t.Errorf("flat remedy %q should recommend installing docling and pandoc", got)
	}
	if strings.Contains(got, "future") {
		t.Errorf("remedy must not call pandoc a 'future' extractor now that #393 ships: %q", got)
	}
	// Neither-engine formats (no installable extractor reads them) → pre-convert,
	// no docling/pandoc suggestion.
	if got := uncoveredExtractionRemedy([]string{".gif", ".svg"}, noEngines, ""); strings.Contains(got, "docling") || strings.Contains(got, "pandoc") {
		t.Errorf("neither-engine remedy %q should not recommend an extractor", got)
	}
	// docling active, pandoc not: an uncovered .odt is pandoc-coverable → suggest
	// pandoc, and do NOT re-suggest the already-active docling.
	if got := uncoveredExtractionRemedy([]string{".odt"}, structuredOnly, ""); !strings.Contains(got, "pandoc") || strings.Contains(got, "install docling") {
		t.Errorf("docling-active remedy %q should recommend pandoc and not re-suggest docling", got)
	}
	// Both engines active: nothing installable helps → pre-convert only.
	if got := uncoveredExtractionRemedy([]string{".odp", ".gif"}, doclingAndPandoc, ""); strings.Contains(got, "install") {
		t.Errorf("both-active remedy %q should only advise pre-conversion", got)
	}
}

// TestUncoveredExtractionRemedy_LegacyDocNamesMissingBinary checks that an
// uncovered legacy .doc gets the libreoffice install hint (spec 0.93.0) that
// names the missing binary, that an active libreoffice engine is never
// re-suggested, and that a pin which excludes the engine names the knob.
func TestUncoveredExtractionRemedy_LegacyDocNamesMissingBinary(t *testing.T) {
	hint := "install soffice (LibreOffice) for legacy .doc"
	got := uncoveredExtractionRemedy([]string{".doc"}, doclingAndPandoc, hint)
	if !strings.Contains(got, hint) {
		t.Errorf("remedy %q should carry the libreoffice hint %q", got, hint)
	}
	if strings.Contains(got, "install pandoc (#393)") {
		t.Errorf("remedy %q must not suggest the already-active pandoc engine", got)
	}
	active := ingest.ActiveEngines{Structured: true, Pandoc: true, LibreOffice: true}
	if got := uncoveredExtractionRemedy([]string{".doc"}, active, ""); strings.Contains(got, "soffice") {
		t.Errorf("remedy %q must not suggest LibreOffice when the engine is active", got)
	}
	if got := uncoveredExtractionRemedy([]string{".doc"}, structuredOnly, ""); !strings.Contains(got, "ingest.extractor") {
		t.Errorf("remedy %q should name ingest.extractor when the binaries exist but a pin excludes the engine", got)
	}
}

// TestLibreOfficeRow_ListsMissingBinaries checks the startup/doctor engine list
// (SPEC §7.7): under `auto` with neither soffice nor pandoc on PATH, the
// libreoffice engine (spec 0.93.0) gets its own "unavailable" row that names
// both missing binaries; a pin that excludes the engine shows no row.
func TestLibreOfficeRow_ListsMissingBinaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	row, ok := libreofficeRow(config.Config{IngestExtractor: "auto"}, ingest.ExtractorDecision{})
	if !ok {
		t.Fatal("auto must list the libreoffice engine")
	}
	if row.Capability != "LibreOffice" || row.Provider != "unavailable" {
		t.Fatalf("row = %+v, want LibreOffice/unavailable", row)
	}
	for _, want := range []string{"soffice", "pandoc", "legacy .doc"} {
		if !strings.Contains(row.Reason, want) {
			t.Errorf("row reason %q must name %q", row.Reason, want)
		}
	}
	if _, ok := libreofficeRow(config.Config{IngestExtractor: "docling"}, ingest.ExtractorDecision{}); ok {
		t.Error("a docling pin must not list the libreoffice engine")
	}
	if _, ok := libreofficeRow(config.Config{IngestExtractor: "libreoffice"}, ingest.ExtractorDecision{Name: "libreoffice"}); ok {
		t.Error("no extra row when libreoffice is the primary (the OCR row names it)")
	}
}
