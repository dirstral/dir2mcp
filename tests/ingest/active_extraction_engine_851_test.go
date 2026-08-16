package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// ActiveExtractionEngine is the source of truth dir2mcp_stats reports through
// models.ocr (issue #851). The daemon takes it ONCE at start, from the live
// pipeline, so the reported engine cannot drift from the engine ingest built and
// no exec or network probe runs inside the polled stats handler.

// extractionEngineService builds a Service with no extractor wired.
func extractionEngineService(t *testing.T) *ingest.Service {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return mustNewIngestService(t, config.Default(), st)
}

// TestActiveExtractionEngine_ReportsTheWiredEngine pins that the accessor names
// the engine actually wired, and that the local engines report no model id: they
// are tools, not models, so a model id there would be an invention.
func TestActiveExtractionEngine_ReportsTheWiredEngine(t *testing.T) {
	for _, tc := range []struct {
		want      string
		extractor model.DocumentExtractor
	}{
		{want: "docling", extractor: ingest.NewDoclingExtractor("")},
		{want: "pandoc", extractor: ingest.NewPandocExtractor("")},
	} {
		t.Run(tc.want, func(t *testing.T) {
			svc := extractionEngineService(t)
			svc.SetDocumentExtractor(tc.extractor)
			engine, ocrModel := svc.ActiveExtractionEngine()
			if engine != tc.want {
				t.Fatalf("engine = %q, want %q", engine, tc.want)
			}
			if ocrModel != "" {
				t.Fatalf("ocrModel = %q, want empty: %s is a local tool with no model id", ocrModel, tc.want)
			}
		})
	}
}

// TestActiveExtractionEngine_ReportsTheBespokeOCRModel pins the other branch: a
// bespoke-OCR backend DOES have a model identity, and it is the model on the
// wire, so stats can keep naming it.
func TestActiveExtractionEngine_ReportsTheBespokeOCRModel(t *testing.T) {
	svc := extractionEngineService(t)
	client := mistral.NewClient("http://127.0.0.1:9", "test-key")
	client.DefaultOCRModel = "house-ocr-v3"
	svc.SetDocumentExtractor(client)

	engine, ocrModel := svc.ActiveExtractionEngine()
	if engine != "mistral" {
		t.Fatalf("engine = %q, want %q", engine, "mistral")
	}
	if ocrModel != "house-ocr-v3" {
		t.Fatalf("ocrModel = %q, want the model on the wire %q", ocrModel, "house-ocr-v3")
	}
}

// TestActiveExtractionEngine_EmptyWhenNothingIsWired pins the honest empty case.
// A pipeline that extracts nothing must not name an engine, so stats can report
// "no extraction engine" instead of a plausible default.
func TestActiveExtractionEngine_EmptyWhenNothingIsWired(t *testing.T) {
	engine, ocrModel := extractionEngineService(t).ActiveExtractionEngine()
	if engine != "" || ocrModel != "" {
		t.Fatalf("engine/model = %q/%q, want both empty with no extractor wired", engine, ocrModel)
	}
}
