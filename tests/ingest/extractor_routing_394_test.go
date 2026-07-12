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

// TestUnsupportedDocument_LenientSkipsHonestly is the #584 guard for the lenient
// DEFAULT (ingest.on_unsupported=lenient): an unsupported .odt with no searchable
// representation is NOT a per-document error, and NOT left status="ok" (which
// mislabeled an unsearchable document as indexed). It is recorded as a DURABLE
// skip — status="skipped" with an unsupported-format skip_reason — so the coverage
// gap survives the run that produced it and status/reindex name it after a restart
// (§7.4.B.2 / §7.7). The document is re-read from the store here, so a persisted
// skip proves the durability property.
func TestUnsupportedDocument_LenientSkipsHonestly(t *testing.T) {
	doc, _ := processWithFlatExtractor(t, "notes.odt", "") // empty = lenient default
	if doc.Status != "skipped" {
		t.Fatalf("unsupported .odt (lenient) status = %q, want \"skipped\" (durable skip, not ok/error) (#584)", doc.Status)
	}
	if doc.SkipReason != model.SkipReasonUnsupportedFormat {
		t.Errorf("skip_reason = %q, want %q so the coverage aggregate names it", doc.SkipReason, model.SkipReasonUnsupportedFormat)
	}
}

// TestUnsupportedDocument_LenientFiresFileSkip is the streaming half of #584: a
// lenient unsupported-format skip must emit a per-document file_skip event (via the
// SetOnDocumentSkip hook) carrying the unsupported_format reason, so a `--json`
// consumer learns the document was left uncovered — not only the durable status.
func TestUnsupportedDocument_LenientFiresFileSkip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.odt"), "irrelevant bytes")
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st) // lenient default
	svc.SetDocumentExtractor(&fakeExtractor{text: "should never be extracted"})

	var gotPath, gotReason string
	svc.SetOnDocumentSkip(func(relPath, _docType, reason string) { gotPath, gotReason = relPath, reason })

	f := ingest.DiscoveredFile{RelPath: "notes.odt", SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if gotPath != "notes.odt" || gotReason != model.SkipReasonUnsupportedFormat {
		t.Errorf("file_skip event = (%q, %q), want (\"notes.odt\", %q)", gotPath, gotReason, model.SkipReasonUnsupportedFormat)
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
