package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// processWithFlatExtractor writes name into a fresh root, ingests it through a
// service whose document extractor is the flat (OCR-only) fakeExtractor under the
// given ingest.on_unsupported mode, and returns the persisted document plus the
// service (so callers can read its in-run skip counts). fakeExtractor does not
// implement structuredExtractor, so it mirrors the Mistral-OCR routing path. An
// empty mode exercises the lenient default.
func processWithFlatExtractor(t *testing.T, name, onUnsupported string) (model.Document, *ingest.Service) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, name), "irrelevant bytes")
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), IngestOnUnsupported: onUnsupported}, st)
	// A flat extractor that would happily return text if asked — proving the doc
	// is routed away by the capability-aware selection, not by the extractor
	// failing.
	svc.SetDocumentExtractor(&fakeExtractor{text: "should never be extracted"})

	f := ingest.DiscoveredFile{RelPath: name, SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", name, err)
	}
	return documentByPath(t, st, name), svc
}

// TestUnsupportedDocument_StrictSurfacesError is the #394 defect-2 guard under
// ingest.on_unsupported=strict: an .odt (doc_type=document) routed to an extractor
// that cannot import it is a non-fatal per-document error (§7.4.B.2) — not a silent
// empty representation, and not an alarming generic provider failure.
func TestUnsupportedDocument_StrictSurfacesError(t *testing.T) {
	doc, _ := processWithFlatExtractor(t, "notes.odt", "strict")
	if doc.Status != "error" {
		t.Fatalf("unsupported .odt (strict) status = %q, want \"error\"", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "unsupported format") {
		t.Errorf("error_message = %q, want it to name the unsupported format", doc.ErrorMessage)
	}
}

// TestUnsupportedImage_StrictSurfacesError is the #394 defect-3 guard under strict:
// a .gif (doc_type=image) is outside the OCR provider's allowlist and covered by no
// extractor. It must surface the same visible unsupported-format error instead of
// being silently rejected.
func TestUnsupportedImage_StrictSurfacesError(t *testing.T) {
	doc, _ := processWithFlatExtractor(t, "diagram.gif", "strict")
	if doc.Status != "error" {
		t.Fatalf("unsupported .gif (strict) status = %q, want \"error\"", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "unsupported format") {
		t.Errorf("error_message = %q, want it to name the unsupported format", doc.ErrorMessage)
	}
}

// TestUnsupportedDocument_LenientSkipsHonestly is the #395 Stage 3 guard for the
// lenient DEFAULT (ingest.on_unsupported=lenient): an unsupported .odt is NOT a
// per-document error — the document is indexed with whatever representations it has
// (none here) and the coverage gap is recorded in the in-run skip counter so
// status/reindex and the honest coverage report name it (§7.4.B.2 / §7.7). The key
// property versus the pre-#395 behavior is that it is honest, not silent: the skip
// reason is counted rather than the format being handed to an engine that can't
// read it.
func TestUnsupportedDocument_LenientSkipsHonestly(t *testing.T) {
	doc, svc := processWithFlatExtractor(t, "notes.odt", "") // empty = lenient default
	if doc.Status == "error" {
		t.Fatalf("unsupported .odt (lenient) status = %q, want a non-error status (lenient skips, not errors)", doc.Status)
	}
	counts := svc.SkipReasonCounts()
	if counts[model.SkipReasonUnsupportedFormat] == 0 {
		t.Errorf("lenient skip did not record an unsupported_format skip reason; counts=%v", counts)
	}
}

// TestSupportedImage_StillExtracted guards against over-reach: a .png the OCR
// provider CAN read must still be extracted normally, not swept up by the routing
// gate, in either degradation mode.
func TestSupportedImage_StillExtracted(t *testing.T) {
	doc, _ := processWithFlatExtractor(t, "scan.png", "")
	if doc.Status != "ok" {
		t.Fatalf("supported .png status = %q, want \"ok\" (the routing gate must not block a readable format)", doc.Status)
	}
}
