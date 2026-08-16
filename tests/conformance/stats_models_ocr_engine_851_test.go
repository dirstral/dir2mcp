package conformance

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/mistral"
)

// models.ocr must name the engine that ACTUALLY extracts (issue #851).
//
// The field used to report the resolved bespoke-OCR profile on every
// deployment. The engine that runs is picked by a cascade whose first three
// stops are docling, docling-serve and pandoc, so a server on
// `ingest.extractor: docling` still answered "mistral-ocr-latest": a model that
// touched no document in the corpus. dir2mcp_stats is where an operator finds
// out what the server does, so a confidently wrong answer there sends the
// operator to inspect the wrong component and to tune a setting nothing reads.
//
// Every assertion below reads the tool response over the production transport,
// which is the payload a client gets, not an internal struct.

// statsOCRField calls dir2mcp_stats and returns models.ocr.
func statsOCRField(t *testing.T, opts ...mcp.ServerOption) string {
	t.Helper()
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	srv := newServer(t, cfg, opts...)
	defer srv.Close()

	structured := callStatsStructured(t, srv, cfg)
	models, ok := structured["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats payload carries no models object: %#v", structured["models"])
	}
	ocr, ok := models["ocr"].(string)
	if !ok {
		t.Fatalf("models.ocr is not a string: %#v", models["ocr"])
	}
	return ocr
}

// TestStatsModelsOCR_NamesTheLocalEngineThatExtracts pins the core of #851: a
// corpus extracted by a local engine MUST NOT report an OCR profile as the
// extractor.
//
// Each case fails without the fix, which answers mistral.DefaultOCRModel for all
// three: the shipped OCR default of a deployment with no OCR credential.
func TestStatsModelsOCR_NamesTheLocalEngineThatExtracts(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"docling", "docling-serve", "pandoc"} {
		t.Run(engine, func(t *testing.T) {
			t.Parallel()
			got := statsOCRField(t, mcp.WithExtractionProvenance(mcp.ExtractionProvenance{Engine: engine}))
			if got == mistral.DefaultOCRModel {
				t.Fatalf("models.ocr = %q with %s extracting: stats names an OCR profile that ran nothing", got, engine)
			}
			if got != engine {
				t.Fatalf("models.ocr = %q, want the engine in effect %q", got, engine)
			}
		})
	}
}

// TestStatsModelsOCR_KeepsTheProfileWhenOCRReallyRuns is the regression guard.
// A deployment whose extraction really is bespoke OCR must keep reporting its
// model id: the fix removes a lie, it does not remove the true answer.
func TestStatsModelsOCR_KeepsTheProfileWhenOCRReallyRuns(t *testing.T) {
	t.Parallel()
	got := statsOCRField(t, mcp.WithExtractionProvenance(mcp.ExtractionProvenance{
		Engine:   "mistral",
		OCRModel: "house-ocr-v3",
	}))
	if got != "house-ocr-v3" {
		t.Fatalf("models.ocr = %q, want the OCR model on the wire %q", got, "house-ocr-v3")
	}
}

// TestStatsModelsOCR_ReportsNoEngineWhenNothingExtracts pins the omit-rather-
// than-guess rule. A daemon that wired no extraction engine (ingest.extractor=off,
// or nothing available) must say so. The spec makes models.ocr required, so the
// field cannot be dropped; it carries an explicit marker instead, and the
// parentheses keep it distinguishable from "OCR runs with profile X".
//
// It fails without the fix, which reports mistral.DefaultOCRModel: a model no
// document ever passed through.
func TestStatsModelsOCR_ReportsNoEngineWhenNothingExtracts(t *testing.T) {
	t.Parallel()
	got := statsOCRField(t, mcp.WithExtractionProvenance(mcp.ExtractionProvenance{}))
	if got == mistral.DefaultOCRModel {
		t.Fatalf("models.ocr = %q with no extraction engine wired: stats names a model that ran nothing", got)
	}
	if got != "(no extraction engine)" {
		t.Fatalf("models.ocr = %q, want the explicit no-engine marker", got)
	}
}

// TestStatsModelsOCR_PayloadStaysCanonicalWithALocalEngine keeps the honest
// answer inside the contract. models.ocr is a required string in the canonical
// §15.6 schema, so an engine name and the no-engine marker must both validate;
// a client that validates every response must not start rejecting them.
func TestStatsModelsOCR_PayloadStaysCanonicalWithALocalEngine(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"docling", "pandoc", ""} {
		t.Run("engine="+engine, func(t *testing.T) {
			t.Parallel()
			cfg := defaultConfig()
			cfg.StateDir = t.TempDir()
			srv := newServer(t, cfg, mcp.WithExtractionProvenance(mcp.ExtractionProvenance{Engine: engine}))
			defer srv.Close()

			assertCanonicalStats(t, callStatsStructured(t, srv, cfg))
		})
	}
}
