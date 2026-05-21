package ingest

import (
	"os/exec"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// ExtractorDecision captures which document extractor will be used and
// why, for diagnostic surfaces (the `dir2mcp up` banner and the
// daemon-side `dir2mcp doctor`). It mirrors the resolution that
// DocumentExtractorFromConfig performs without constructing an
// extractor, so callers that only need to explain the choice don't pay
// the construction cost.
type ExtractorDecision struct {
	// Name is the selected extractor identifier: "docling",
	// "mistral-ocr", or "" when no extractor is available.
	Name string
	// Source describes how the choice was made: "explicit" (user
	// pinned via ingest.extractor), "auto" (auto-detected as the
	// preferred path), "fallback" (preferred path unavailable),
	// or "disabled" (no extractor will run).
	Source string
	// Reason is a human-readable explanation, especially useful
	// when the choice is not the most desirable one (e.g. fallback
	// to Mistral OCR because docling is not on PATH).
	Reason string
}

// DescribeDocumentExtractor returns the selection result for cfg
// without building an extractor. It MUST stay in lockstep with
// DocumentExtractorFromConfig so the banner reflects what the runtime
// will actually use.
func DescribeDocumentExtractor(cfg config.Config) ExtractorDecision {
	mode := strings.ToLower(strings.TrimSpace(cfg.IngestExtractor))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "off":
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=off"}
	case "docling":
		if tpl := strings.TrimSpace(cfg.DoclingCommand); tpl != "" {
			return ExtractorDecision{Name: "docling", Source: "explicit", Reason: "configured docling command: " + tpl}
		}
		if path, err := exec.LookPath("docling"); err == nil {
			return ExtractorDecision{Name: "docling", Source: "explicit", Reason: "auto-detected at " + path}
		}
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=docling but docling command is unavailable"}
	case "mistral":
		if mistralOCRAvailable(cfg) {
			return ExtractorDecision{Name: "mistral-ocr", Source: "explicit"}
		}
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=mistral but the mistral-ocr provider has no credential"}
	default: // auto
		if tpl := strings.TrimSpace(cfg.DoclingCommand); tpl != "" {
			return ExtractorDecision{Name: "docling", Source: "auto", Reason: "configured docling command: " + tpl}
		}
		if path, err := exec.LookPath("docling"); err == nil {
			return ExtractorDecision{Name: "docling", Source: "auto", Reason: "auto-detected at " + path}
		}
		if mistralOCRAvailable(cfg) {
			return ExtractorDecision{Name: "mistral-ocr", Source: "fallback", Reason: "docling not found on PATH; falling back to Mistral OCR"}
		}
		return ExtractorDecision{Source: "disabled", Reason: "no extractor available: docling not on PATH and no Mistral credential"}
	}
}

// mistralOCRAvailable reports whether the mistral-ocr provider profile
// resolves to a usable credential. Used by DescribeDocumentExtractor to
// distinguish "fallback selected" from "no extractor at all".
func mistralOCRAvailable(cfg config.Config) bool {
	_, err := cfg.Providers().ResolveExplicit(provider.CapOCR, "mistral-ocr", true)
	return err == nil
}
