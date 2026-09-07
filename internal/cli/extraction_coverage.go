package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// extractionCoverage is the SPEC §7.7 honest-coverage verdict: the corpus format
// classes present in the durable document record that no ACTIVE extraction
// engine reads, plus the remediation that names the engine or config to add. It
// is computed once and rendered by two surfaces, the `dir2mcp up` banner and the
// doctor's extraction_coverage check, so the two can never disagree about which
// formats are uncovered (#395).
type extractionCoverage struct {
	// Engine is the primary extraction engine of the cascade ("" when none).
	Engine string
	// Uncovered is the sorted list of extensions present in the record that no
	// active engine reads under the ingest.extractor policy.
	Uncovered []string
	// Docs is how many non-deleted extractable documents those extensions hold.
	Docs int64
	// Remedy names what to add (an engine, a credential, a config value).
	Remedy string
}

// extractableExtensionCounter is the store capability the coverage verdict reads:
// per-extension counts of non-deleted pdf/image/document rows. The shipped SQLite
// store implements it; a store that does not (a test fake) simply yields no
// coverage section rather than an error.
type extractableExtensionCounter interface {
	ExtractableExtensionCounts(ctx context.Context, status string) (map[string]int64, error)
}

// computeExtractionCoverage derives the §7.7 verdict for cfg from the durable
// document record and the resolved primary-engine decision.
//
// Presence is decided over EVERY non-deleted extractable document, regardless of
// status (§7.7, spec 0.60.1). An uncovered document is by construction recorded
// as status="skipped" (lenient, #584) or status="error" (strict), so a verdict
// that counted only status="ok" rows went blind the moment the gap was recorded
// honestly: measured on main, `doctor` reported the coverage check healthy with
// an .odt durably skipped and a .tiff errored in the store. The empty status
// filter is that fix.
//
// Which formats count as covered is the SAME per-format router indexing uses
// (ingest.ExtractionCovered → selectExtractionRoute), including the pandoc (T2)
// tier, so the report names exactly the formats indexing skips.
func computeExtractionCoverage(ctx context.Context, counter extractableExtensionCounter, cfg config.Config, decision ingest.ExtractorDecision) (extractionCoverage, error) {
	extCounts, err := counter.ExtractableExtensionCounts(ctx, "")
	if err != nil {
		return extractionCoverage{}, err
	}
	structured := extractorIsStructured(decision.Name)
	flatOCR := decision.Name == "mistral-ocr"
	// pandoc (T2, #393) can be a second active engine under `auto` (and is the
	// primary under its pin); fold its availability in so a format it covers is
	// never reported as a gap.
	pandocActive := ingest.PandocActive(cfg)
	uncovered, docs := uncoveredExtractableExtensions(extCounts, cfg.IngestExtractor, structured, flatOCR, pandocActive)
	cov := extractionCoverage{Engine: decision.Name, Uncovered: uncovered, Docs: docs}
	if len(uncovered) == 0 {
		return cov, nil
	}
	if strings.EqualFold(strings.TrimSpace(cfg.IngestExtractor), "off") {
		// The operator switched extraction off: every extractable format is
		// uncovered by choice. Name the knob rather than an engine to install.
		cov.Remedy = "ingest.extractor is off; set it to auto (or pin an engine) to extract these formats."
		return cov, nil
	}
	cov.Remedy = uncoveredExtractionRemedy(uncovered, structured, pandocActive)
	if decision.Name == "" {
		// No primary engine resolved at all: the cascade's own reason ("docling
		// not found on PATH, no docling-serve URL, and no Mistral credential") is
		// the first thing to fix, so lead with it.
		cov.Remedy = "No extractor is available (" + decision.Reason + "). " + cov.Remedy
	}
	return cov, nil
}

// startupExtractionCoverage computes the coverage verdict for the `up` banner.
// It is silent on the paths that print no banner (--json, --quiet) and when the
// store cannot count extensions; a read failure is reported on stderr rather
// than swallowed, because a coverage report that fails quietly is the silence
// §7.7 forbids.
func startupExtractionCoverage(ctx context.Context, st interface{}, cfg config.Config, opts upOptions, stderr io.Writer) extractionCoverage {
	if opts.jsonOutput || opts.quiet {
		return extractionCoverage{}
	}
	counter, ok := st.(extractableExtensionCounter)
	if !ok {
		return extractionCoverage{}
	}
	decision := ingest.DescribeDocumentExtractorContext(ctx, cfg)
	cov, err := computeExtractionCoverage(ctx, counter, cfg, decision)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: extraction coverage unavailable: %v\n", err)
		return extractionCoverage{}
	}
	return cov
}

// printCoverageSection emits the banner's "Coverage" section: the uncovered
// formats the durable record holds, how many documents they account for, and the
// remediation (SPEC §7.7 startup diagnostics). Silent when every present format
// is covered, so a healthy corpus keeps the banner clean. On a first run the
// record is empty until the scan has recorded the corpus; the gap that scan finds
// is recorded durably (§7.4.B.2) and this section names it on the next start,
// while `dir2mcp status` and `dir2mcp doctor` name it immediately.
func printCoverageSection(out io.Writer, s styles, cov extractionCoverage) {
	if len(cov.Uncovered) == 0 {
		return
	}
	writef(out, "  %s\n", s.sectionHeader("Coverage"))
	writeln(out, s.kv("Uncovered", fmt.Sprintf("%s %s",
		strings.Join(cov.Uncovered, ", "),
		s.dim(fmt.Sprintf("(%d document(s); no active engine reads them; recorded as skipped or error, never as indexed)", cov.Docs)))))
	writeln(out, s.kv("Fix", cov.Remedy))
	writeln(out)
}
