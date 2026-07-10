package ingest

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestActiveOCRIdentityForPath_PandocRouteUsesPandocIdentity is the regression for
// the perpetual-staleness bug (#393): under `ingest.extractor: auto` two engines
// can be active (docling primary + capability-activated pandoc). A born-digital
// doc (.odt) is extracted by pandoc and records provider "pandoc" in its meta_json,
// so the derivation-identity staleness check (ocrStale) MUST compare it against the
// pandoc identity, not the primary docling's — otherwise recorded "pandoc" never
// matches active "docling" and the doc re-extracts on every run.
func TestActiveOCRIdentityForPath_PandocRouteUsesPandocIdentity(t *testing.T) {
	s := &Service{
		cfg:             config.Config{IngestExtractor: "auto"},
		extractor:       NewDoclingExtractor("docling"), // structured primary (T1)
		pandocExtractor: NewPandocExtractor("pandoc"),   // second engine (T2)
	}

	// .odt is docling-unreadable, so under auto it routes to pandoc: the active
	// identity must equal what a stored pandoc extraction records.
	wantPandoc, ok := ocrIdentityFromMeta(`{"provider":"pandoc"}`)
	if !ok {
		t.Fatal("ocrIdentityFromMeta rejected a pandoc meta_json")
	}
	gotODT := s.activeOCRIdentityForPath("report.odt")
	if gotODT != wantPandoc {
		t.Errorf("active OCR identity for .odt = %q, want the pandoc identity %q", gotODT, wantPandoc)
	}

	// The primary (docling) identity must NOT be what an .odt compares against —
	// that mismatch was the bug.
	if gotODT == s.activeOCRIdentity() {
		t.Errorf("active OCR identity for .odt must not be the primary docling identity %q", s.activeOCRIdentity())
	}

	// A docling-readable format (.pdf) still resolves to the primary (docling)
	// identity, unchanged by the pandoc wiring.
	if gotPDF := s.activeOCRIdentityForPath("scan.pdf"); gotPDF != s.activeOCRIdentity() {
		t.Errorf("active OCR identity for .pdf = %q, want the primary identity %q", gotPDF, s.activeOCRIdentity())
	}

	// A format no active engine can read (.doc: docling-denied, not pandoc-readable)
	// has no active OCR identity, so the staleness check self-skips (fail-open).
	if got := s.activeOCRIdentityForPath("legacy.doc"); got != "" {
		t.Errorf("active OCR identity for uncovered .doc = %q, want empty", got)
	}
}
