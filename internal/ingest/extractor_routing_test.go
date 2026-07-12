package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mistral"
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

// extractorRoutingCases is the full #394/#395 fixture set. It is the regression
// guard proving the consolidated capability table (internal/ingest/capability.go)
// returns byte-identical verdicts to the pre-consolidation inline allowlists: the
// flat OCR path reads exactly the ocrMIMEType allowlist, docling additionally
// reads OpenXML Office and tiff/bmp but not the OpenDocument/RTF/legacy-.doc
// family or gif/svg, and any unlisted extension (html, txt) stays "supported" on
// the structured denylist path.
var extractorRoutingCases = []struct {
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
	// Read by neither the flat OCR nor the docling engine. Some are now covered by
	// pandoc (T2, #393) — see TestSelectExtractionRoute_Pandoc — but the flat/
	// structured verdicts here are unchanged. .epub is docling-unreadable (#393
	// routes it to pandoc).
	{".odt", false, false},
	{".epub", false, false},
	{".odp", false, false},
	{".ods", false, false},
	{".rtf", false, false},
	{".doc", false, false},
	{".gif", false, false},
	{".svg", false, false},
	// Unlisted extensions: rejected on the flat allowlist, "supported" on the
	// structured denylist (the old `default: return true` branch — e.g. html
	// reaches docling; #394 defect 1).
	{".html", false, true},
	{".txt", false, true},
}

// TestExtractorSupportsExt pins the #394 capability split against the
// consolidated table, and asserts the exported wrapper agrees with the
// unexported form so out-of-package diagnostics see identical verdicts.
func TestExtractorSupportsExt(t *testing.T) {
	for _, tc := range extractorRoutingCases {
		if got := extractorSupportsExt(false, tc.ext); got != tc.flat {
			t.Errorf("extractorSupportsExt(flat, %q) = %v, want %v", tc.ext, got, tc.flat)
		}
		if got := extractorSupportsExt(true, tc.ext); got != tc.structured {
			t.Errorf("extractorSupportsExt(structured, %q) = %v, want %v", tc.ext, got, tc.structured)
		}
		if got := ExtractorSupportsExt(false, tc.ext); got != tc.flat {
			t.Errorf("ExtractorSupportsExt(flat, %q) = %v, want %v", tc.ext, got, tc.flat)
		}
		if got := ExtractorSupportsExt(true, tc.ext); got != tc.structured {
			t.Errorf("ExtractorSupportsExt(structured, %q) = %v, want %v", tc.ext, got, tc.structured)
		}
	}
}

// TestOCRMIMESetMatchesCapabilityTable guards the mirror between the flat-OCR
// verdict in the consolidated capability table and mistral.ocrMIMEType's
// supported-extension set (the two authoritative-but-cycle-separated allowlists
// #395 Stage 1 consolidates). Any divergence — an extension one accepts and the
// other rejects — would reintroduce the silent-routing class of bug and fails
// here.
func TestOCRMIMESetMatchesCapabilityTable(t *testing.T) {
	for _, tc := range extractorRoutingCases {
		if got := mistral.SupportsOCRExt(tc.ext); got != tc.flat {
			t.Errorf("mistral.SupportsOCRExt(%q) = %v, but capability table flat verdict is %v; the OCR MIME set and the table have drifted", tc.ext, got, tc.flat)
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

// TestSelectExtractionRoute is the #395 Stage 2 / #556 guard for the
// capability-aware, per-format selection (SPEC §7.4.B.1 best-available + §7.4.A
// markup boundary). It pins the fidelity-ordered choice for every combination of
// policy, active engines, and format that the shipped engines can produce.
func TestSelectExtractionRoute(t *testing.T) {
	both := extractionAvailability{structured: true, flatOCR: true}
	structuredOnly := extractionAvailability{structured: true}
	flatOnly := extractionAvailability{flatOCR: true}
	none := extractionAvailability{}

	cases := []struct {
		name   string
		policy string
		avail  extractionAvailability
		ext    string
		want   extractionRoute
	}{
		// auto: docling (T1) is preferred whenever active, for every format it reads.
		{"auto both pdf -> structured (T1 wins)", "auto", both, ".pdf", routeStructured},
		{"auto both docx -> structured", "auto", both, ".docx", routeStructured},
		{"auto both tiff -> structured (mistral cannot)", "auto", both, ".tiff", routeStructured},
		// auto: docling absent -> mistral (T3) for its allowlist, degrade otherwise.
		{"auto flat pdf -> flat OCR", "auto", flatOnly, ".pdf", routeFlatOCR},
		{"auto flat png -> flat OCR", "auto", flatOnly, ".png", routeFlatOCR},
		{"auto flat docx -> degrade (mistral cannot read docx)", "auto", flatOnly, ".docx", routeDegrade},
		{"auto flat tiff -> degrade", "auto", flatOnly, ".tiff", routeDegrade},
		// auto: nothing active -> degrade (except html, see below).
		{"auto none pdf -> degrade", "auto", none, ".pdf", routeDegrade},
		// html markup boundary (#556 / §7.4.A):
		{"auto structured html -> structured (promoted, #556)", "auto", structuredOnly, ".html", routeStructured},
		{"auto structured htm -> structured", "auto", structuredOnly, ".htm", routeStructured},
		{"auto flat html -> raw_text baseline (mistral cannot read html)", "auto", flatOnly, ".html", routeRawText},
		{"auto none html -> raw_text baseline (never dropped)", "auto", none, ".html", routeRawText},
		// pinned docling: only structured is eligible.
		{"pin docling pdf -> structured", "docling", structuredOnly, ".pdf", routeStructured},
		{"pin docling html -> structured", "docling", structuredOnly, ".html", routeStructured},
		{"pin docling odt -> degrade (docling cannot import odt)", "docling", structuredOnly, ".odt", routeDegrade},
		// pinned mistral: no cross-engine fallback; html still keeps its baseline.
		{"pin mistral pdf -> flat OCR", "mistral", flatOnly, ".pdf", routeFlatOCR},
		{"pin mistral docx -> degrade (no fallback to docling)", "mistral", both, ".docx", routeDegrade},
		{"pin mistral html -> raw_text baseline (§7.4.A, never dropped)", "mistral", both, ".html", routeRawText},
		// docling-serve is a structured transport: behaves like docling for selection.
		{"pin docling-serve pdf -> structured", "docling-serve", structuredOnly, ".pdf", routeStructured},
		// off: no extraction engine eligible; html still falls to its baseline.
		{"off pdf -> degrade", "off", both, ".pdf", routeDegrade},
		{"off html -> raw_text baseline", "off", both, ".html", routeRawText},
		// empty policy defaults to auto.
		{"empty policy defaults to auto", "", both, ".pdf", routeStructured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectExtractionRoute(tc.policy, tc.avail, tc.ext); got != tc.want {
				t.Errorf("selectExtractionRoute(%q, %+v, %q) = %d, want %d", tc.policy, tc.avail, tc.ext, got, tc.want)
			}
		})
	}
}

// TestSelectExtractionRoute_Pandoc is the #393 guard for the T2 pandoc tier: it
// pins the fidelity-ordered choice for pandoc-readable born-digital formats across
// the auto policy, the pandoc pin, and the docling pin (which must NOT borrow
// pandoc).
func TestSelectExtractionRoute_Pandoc(t *testing.T) {
	doclingAndPandoc := extractionAvailability{structured: true, pandoc: true}
	pandocOnly := extractionAvailability{pandoc: true}
	doclingOnly := extractionAvailability{structured: true}

	cases := []struct {
		name   string
		policy string
		avail  extractionAvailability
		ext    string
		want   extractionRoute
	}{
		// auto, docling + pandoc both active: docling (T1) wins for what it reads;
		// pandoc (T2) covers the docling-unreadable born-digital family.
		{"auto docling+pandoc docx -> structured (T1 wins)", "auto", doclingAndPandoc, ".docx", routeStructured},
		{"auto docling+pandoc odt -> pandoc (docling cannot import)", "auto", doclingAndPandoc, ".odt", routePandoc},
		{"auto docling+pandoc rtf -> pandoc", "auto", doclingAndPandoc, ".rtf", routePandoc},
		{"auto docling+pandoc epub -> pandoc (docling has no epub reader)", "auto", doclingAndPandoc, ".epub", routePandoc},
		{"auto docling+pandoc pdf -> structured (pandoc reads no pdf)", "auto", doclingAndPandoc, ".pdf", routeStructured},
		// auto, pandoc the only active engine: it reads the OOXML/born-digital it can.
		{"auto pandoc-only docx -> pandoc", "auto", pandocOnly, ".docx", routePandoc},
		{"auto pandoc-only odt -> pandoc", "auto", pandocOnly, ".odt", routePandoc},
		{"auto pandoc-only pdf -> degrade (pandoc reads no pdf)", "auto", pandocOnly, ".pdf", routeDegrade},
		{"auto pandoc-only pptx -> degrade (pandoc has no pptx reader)", "auto", pandocOnly, ".pptx", routeDegrade},
		{"auto pandoc-only xlsx -> degrade (pandoc has no xlsx reader)", "auto", pandocOnly, ".xlsx", routeDegrade},
		{"auto pandoc-only doc -> degrade (pandoc reads docx not legacy doc)", "auto", pandocOnly, ".doc", routeDegrade},
		// pandoc pin: only pandoc eligible; pptx/xlsx/pdf/doc degrade.
		{"pin pandoc odt -> pandoc", "pandoc", pandocOnly, ".odt", routePandoc},
		{"pin pandoc docx -> pandoc", "pandoc", pandocOnly, ".docx", routePandoc},
		{"pin pandoc pptx -> degrade", "pandoc", pandocOnly, ".pptx", routeDegrade},
		{"pin pandoc pdf -> degrade", "pandoc", pandocOnly, ".pdf", routeDegrade},
		// pandoc pin must not borrow docling even when docling is available.
		{"pin pandoc + docling avail docx -> pandoc (no docling borrow)", "pandoc", doclingAndPandoc, ".docx", routePandoc},
		{"pin pandoc + docling avail pdf -> degrade (pandoc reads no pdf, no docling borrow)", "pandoc", doclingAndPandoc, ".pdf", routeDegrade},
		// docling pin must NOT route pandoc formats to pandoc.
		{"pin docling odt -> degrade (no pandoc borrow)", "docling", doclingAndPandoc, ".odt", routeDegrade},
		{"pin docling pptx -> structured (docling reads pptx)", "docling", doclingOnly, ".pptx", routeStructured},
		// html markup boundary with pandoc: pandoc reads html when structured absent.
		{"auto pandoc-only html -> pandoc", "auto", pandocOnly, ".html", routePandoc},
		{"auto pandoc-only+nothing html still never dropped elsewhere", "auto", pandocOnly, ".htm", routePandoc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectExtractionRoute(tc.policy, tc.avail, tc.ext); got != tc.want {
				t.Errorf("selectExtractionRoute(%q, %+v, %q) = %d, want %d", tc.policy, tc.avail, tc.ext, got, tc.want)
			}
		})
	}
}

// TestActiveExtractionAvailability confirms availability is derived from the
// resolved single extractor: a structured extractor reports structured-active, a
// flat one reports flat-active, and no extractor reports nothing active.
func TestActiveExtractionAvailability(t *testing.T) {
	if got := (&Service{extractor: structuredRoutingExtractor{}}).activeExtractionAvailability(); !got.structured || got.flatOCR {
		t.Errorf("structured extractor availability = %+v, want {structured:true}", got)
	}
	if got := (&Service{extractor: flatRoutingExtractor{}}).activeExtractionAvailability(); got.structured || !got.flatOCR {
		t.Errorf("flat extractor availability = %+v, want {flatOCR:true}", got)
	}
	if got := (&Service{}).activeExtractionAvailability(); got.structured || got.flatOCR {
		t.Errorf("nil extractor availability = %+v, want zero", got)
	}
	// #393: pandoc as a SECOND engine alongside a structured primary.
	if got := (&Service{extractor: structuredRoutingExtractor{}, pandocExtractor: NewPandocExtractor("")}).activeExtractionAvailability(); !got.structured || !got.pandoc || got.flatOCR {
		t.Errorf("structured+pandoc availability = %+v, want {structured,pandoc}", got)
	}
	// pandoc as the PRIMARY (pin / auto-only-pandoc): not mislabeled flat/structured.
	pe := NewPandocExtractor("")
	if got := (&Service{extractor: pe, pandocExtractor: pe}).activeExtractionAvailability(); got.structured || got.flatOCR || !got.pandoc {
		t.Errorf("pandoc-primary availability = %+v, want {pandoc} only", got)
	}
}

// TestExtractorCanReadExt_PandocOnlyPrimaryNil is the regression for the optibot
// blocker on #585/#587: the index-time readability guard must be routing-aware —
// when pandoc is the only active engine (primary s.extractor nil, s.pandocExtractor
// set), a pandoc-routed born-digital format is still readable, and a format no
// engine covers is not. Mirrors CanExtractSourceText so the index and annotation
// paths agree.
func TestExtractorCanReadExt_PandocOnlyPrimaryNil(t *testing.T) {
	s := &Service{cfg: config.Config{IngestExtractor: "auto"}, pandocExtractor: NewPandocExtractor("")}
	if !s.extractorCanReadExt("report.odt") {
		t.Error("pandoc-only (nil primary): .odt must be readable via the pandoc route")
	}
	if s.extractorCanReadExt("scan.pdf") {
		t.Error("pandoc-only (nil primary): .pdf reads by no active engine and must not be readable")
	}
	// No engine at all → not readable.
	if (&Service{cfg: config.Config{IngestExtractor: "auto"}}).extractorCanReadExt("report.odt") {
		t.Error("no extractor: .odt must not be readable")
	}
}
