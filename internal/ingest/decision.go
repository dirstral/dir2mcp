package ingest

import (
	"context"
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
	// "docling-serve", "mistral-ocr", or "" when no extractor is available.
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
// will actually use. For docling-serve, availability includes endpoint
// reachability per spec 0.10.0 §7.4.B.
//
// This convenience form uses a background context; the docling-serve
// reachability probe it may run is therefore uncancellable and can block
// for up to the probe timeout. Hot paths that already hold a request
// context (e.g. MCP tool handlers) should call
// DescribeDocumentExtractorContext instead.
func DescribeDocumentExtractor(cfg config.Config) ExtractorDecision {
	return describeDocumentExtractor(context.Background(), cfg)
}

// DescribeDocumentExtractorContext is DescribeDocumentExtractor with a
// caller-provided context so the docling-serve reachability probe honours
// cancellation and deadlines.
func DescribeDocumentExtractorContext(ctx context.Context, cfg config.Config) ExtractorDecision {
	return describeDocumentExtractor(ctx, cfg)
}

func describeDocumentExtractor(ctx context.Context, cfg config.Config) ExtractorDecision {
	mode := strings.ToLower(strings.TrimSpace(cfg.IngestExtractor))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "off":
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=off"}
	case "docling":
		return describeExplicitDocling(ctx, cfg)
	case "docling-serve":
		return describeExplicitDoclingServe(ctx, cfg)
	case "mistral":
		if mistralOCRAvailable(cfg) {
			return ExtractorDecision{Name: "mistral-ocr", Source: "explicit"}
		}
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=mistral but the mistral-ocr provider has no credential"}
	default: // auto
		return describeAutoDocumentExtractor(ctx, cfg)
	}
}

func describeExplicitDoclingServe(ctx context.Context, cfg config.Config) ExtractorDecision {
	serveURL := strings.TrimSpace(cfg.IngestDoclingServeURL)
	switch {
	case serveURL == "":
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=docling-serve but ingest.docling.serve_url is empty"}
	case ProbeDoclingServe(ctx, serveURL) == nil:
		return ExtractorDecision{Name: "docling-serve", Source: "explicit", Reason: "configured docling-serve endpoint"}
	default:
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=docling-serve but the docling-serve endpoint is unreachable"}
	}
}

// describeExplicitDocling resolves the docling CLI for ingest.extractor=docling.
// Per spec 0.15.0 §7.4 the extractor is available only when it both resolves
// AND passes a functional check; a present-but-broken docling disables
// extraction (no silent fallback to another engine), mirroring explicit
// docling-serve.
func describeExplicitDocling(ctx context.Context, cfg config.Config) ExtractorDecision {
	bin, source, ok := resolveDoclingBinary(cfg)
	if !ok {
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=docling but docling command is unavailable"}
	}
	if err := doclingFunctionalCheck(ctx, bin); err != nil {
		return ExtractorDecision{Source: "disabled", Reason: "ingest.extractor=docling but the docling command is present yet failed its functional check"}
	}
	return ExtractorDecision{Name: "docling", Source: "explicit", Reason: doclingResolvedReason(source)}
}

func describeAutoDocumentExtractor(ctx context.Context, cfg config.Config) ExtractorDecision {
	if bin, source, ok := resolveDoclingBinary(cfg); ok {
		if err := doclingFunctionalCheck(ctx, bin); err == nil {
			return ExtractorDecision{Name: "docling", Source: "auto", Reason: doclingResolvedReason(source)}
		}
		// Resolved but non-functional: skip it and continue the cascade so a
		// broken docling install degrades gracefully (spec 0.15.0 §7.4).
		return describeAutoWithoutDoclingCLI(ctx, cfg, "docling CLI present but failed its functional check")
	}
	return describeAutoWithoutDoclingCLI(ctx, cfg, "docling not found on PATH")
}

// describeAutoWithoutDoclingCLI continues the auto cascade when the docling CLI
// is unavailable. doclingReason explains why (not on PATH, or present but
// non-functional) and prefixes the fallback reasons.
func describeAutoWithoutDoclingCLI(ctx context.Context, cfg config.Config, doclingReason string) ExtractorDecision {
	serveURL := strings.TrimSpace(cfg.IngestDoclingServeURL)
	switch {
	case serveURL != "" && ProbeDoclingServe(ctx, serveURL) == nil:
		return ExtractorDecision{Name: "docling-serve", Source: "auto", Reason: doclingReason + "; using configured docling-serve endpoint"}
	case mistralOCRAvailable(cfg) && serveURL != "":
		return ExtractorDecision{Name: "mistral-ocr", Source: "fallback", Reason: doclingReason + "; docling-serve endpoint unreachable; falling back to Mistral OCR"}
	case mistralOCRAvailable(cfg):
		return ExtractorDecision{Name: "mistral-ocr", Source: "fallback", Reason: doclingReason + "; falling back to Mistral OCR"}
	case serveURL != "":
		return ExtractorDecision{Source: "disabled", Reason: doclingReason + "; docling-serve endpoint unreachable; and no Mistral credential"}
	default:
		return ExtractorDecision{Source: "disabled", Reason: "no extractor available: " + doclingReason + ", no docling-serve URL, and no Mistral credential"}
	}
}

// The literal value of cfg.DoclingCommand is intentionally NOT
// embedded in Reason: a user-supplied command template may carry
// credential-bearing flags (e.g. `docling --api-key …`), and Reason
// flows through the startup banner and the support-bundle's
// routing.json. Including the value would compromise the
// "no secrets in diagnostics" contract enforced by the support-bundle
// tests. The exec.LookPath result is also redacted for symmetry.

// mistralOCRAvailable reports whether the mistral-ocr provider profile
// resolves to a usable credential. Used by DescribeDocumentExtractor to
// distinguish "fallback selected" from "no extractor at all".
func mistralOCRAvailable(cfg config.Config) bool {
	_, err := cfg.Providers().ResolveExplicit(provider.CapOCR, "mistral-ocr", true)
	return err == nil
}
