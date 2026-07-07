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
// service whose document extractor is the flat (OCR-only) fakeExtractor, and
// returns the persisted document. fakeExtractor does not implement
// structuredExtractor, so it mirrors the Mistral-OCR routing path.
func processWithFlatExtractor(t *testing.T, name string) model.Document {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, name), "irrelevant bytes")
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	// A flat extractor that would happily return text if asked — proving the doc
	// is skipped by the routing gate, not by the extractor failing.
	svc.SetDocumentExtractor(&fakeExtractor{text: "should never be extracted"})

	f := ingest.DiscoveredFile{RelPath: name, SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", name, err)
	}
	return documentByPath(t, st, name)
}

// TestUnsupportedDocument_SurfacesDiagnostic is the #394 defect-2 guard: an .odt
// (doc_type=document) routed to an extractor that cannot import it must surface a
// clear unsupported-format diagnostic — not a silent empty representation, and
// not an alarming generic provider failure.
func TestUnsupportedDocument_SurfacesDiagnostic(t *testing.T) {
	doc := processWithFlatExtractor(t, "notes.odt")
	if doc.Status != "error" {
		t.Fatalf("unsupported .odt status = %q, want \"error\" (a silent empty rep is the #394 bug)", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "unsupported format") {
		t.Errorf("error_message = %q, want it to name the unsupported format", doc.ErrorMessage)
	}
}

// TestUnsupportedImage_SurfacesDiagnostic is the #394 defect-3 guard: a .gif
// (doc_type=image) is outside the OCR provider's allowlist and covered by no
// extractor. It must surface the same visible unsupported-format diagnostic
// instead of being silently rejected.
func TestUnsupportedImage_SurfacesDiagnostic(t *testing.T) {
	doc := processWithFlatExtractor(t, "diagram.gif")
	if doc.Status != "error" {
		t.Fatalf("unsupported .gif status = %q, want \"error\" (silent OCR rejection is the #394 bug)", doc.Status)
	}
	if !strings.Contains(strings.ToLower(doc.ErrorMessage), "unsupported format") {
		t.Errorf("error_message = %q, want it to name the unsupported format", doc.ErrorMessage)
	}
}

// TestSupportedImage_StillExtracted guards against over-reach: a .png the OCR
// provider CAN read must still be extracted normally, not swept up by the #394
// routing gate.
func TestSupportedImage_StillExtracted(t *testing.T) {
	doc := processWithFlatExtractor(t, "scan.png")
	if doc.Status != "ok" {
		t.Fatalf("supported .png status = %q, want \"ok\" (the routing gate must not block a readable format)", doc.Status)
	}
}
