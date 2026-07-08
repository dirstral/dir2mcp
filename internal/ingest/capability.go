package ingest

import "strings"

// This file is the single internal source of truth for which extraction engine
// can read which file format. It consolidates the three format allowlists that
// were previously scattered across the codebase (#395 Stage 1):
//
//   - ingest.extractorSupportsExt — the docling-structured vs flat-OCR split
//     added by #394 (formerly a pair of inline switch statements here);
//   - mistral.ocrMIMEType — the flat-OCR pdf/png/jpg/jpeg/webp allowlist (its
//     supported-extension set MUST mirror flatOCRReadableExt below; the mirror
//     is enforced by TestOCRMIMESetMatchesCapabilityTable);
//   - docling's implicit supported set — the OpenXML Office + tiff/bmp raster
//     formats it imports, minus the OpenDocument/RTF/legacy-.doc/gif/svg family
//     it cannot.
//
// IMPORTANT (scope): this is a byte-identical refactor. The table reproduces the
// EXACT verdicts the previous inline allowlists returned — it does NOT change
// which engine handles which format, and it deliberately adds NO fallback or
// per-format selection logic. Capability-aware per-format SELECTION (picking the
// best available engine per format, strict/lenient degradation) is #395
// Stages 2-3, which are spec-PR-first (SPEC §7.4.B currently defines extraction
// as a single global cascade, not a provider capability) and OUT OF SCOPE here.
// A per-engine fidelity/precedence column is intentionally deferred to that work
// so nothing in this PR can consume it for routing.

// extractionEngine is the coarse engine family the capability table distinguishes.
// It matches the ONLY distinction extractorCanReadExt makes today: the flat
// Mistral-OCR path vs the structured docling family (docling CLI / docling-serve,
// the engines that emit a DoclingDocument). Bespoke/self-hosted OCR profiles ride
// the flat path; a future pandoc extractor (#393) would be added as a new engine
// here rather than as another scattered allowlist.
type extractionEngine int

const (
	// engineFlatOCR is the flat Mistral-OCR / bespoke-OCR path: text only, no
	// structure. It reads exactly flatOCRReadableExt.
	engineFlatOCR extractionEngine = iota
	// engineStructured is the docling family: it emits a DoclingDocument and
	// imports every pdf/image/document format EXCEPT structuredUnreadableExt.
	engineStructured
)

// flatOCRReadableExt is the exact set of extensions the flat OCR path can read —
// identical to (and the authoritative mirror of) mistral.ocrMIMEType's keys.
// Everything else routed to a flat-OCR extractor is rejected upstream (#394
// defect 3), so it is skipped with a visible unsupported-format diagnostic
// rather than handed to an engine that hard-errors on it.
var flatOCRReadableExt = map[string]struct{}{
	".pdf":  {},
	".png":  {},
	".jpg":  {},
	".jpeg": {},
	".webp": {},
}

// structuredUnreadableExt is the set of pdf/image/document formats the docling
// family CANNOT import: the OpenDocument family (.odt/.odp/.ods), RTF, legacy
// binary .doc, and gif/svg (#394 defects 2 & 3). docling reads every OTHER
// extension routed to it (the shared pdf/png/jpg/jpeg/webp set, plus OpenXML
// Office docx/pptx/xlsx and tiff/bmp raster images), so the structured verdict
// is a denylist: supported unless listed here. Content support for these
// unreadable formats is tracked in #393.
var structuredUnreadableExt = map[string]struct{}{
	".odt": {},
	".odp": {},
	".ods": {},
	".rtf": {},
	".doc": {},
	".gif": {},
	".svg": {},
}

// engineSupportsExt reports whether the given extraction engine can read ext.
// It is the single lookup both extractorSupportsExt and the doctor coverage
// diagnostic consult. ext is expected already lowercased with its leading dot
// (as filepath.Ext returns for a lowercased path).
//
// The verdicts are byte-identical to the pre-consolidation inline allowlists:
//   - flat OCR: an allowlist — true iff ext is in flatOCRReadableExt;
//   - structured: a denylist — true unless ext is in structuredUnreadableExt,
//     so unlisted extensions (e.g. .txt, .html) stay "supported", exactly as the
//     old `default: return true` branch behaved.
func engineSupportsExt(engine extractionEngine, ext string) bool {
	switch engine {
	case engineFlatOCR:
		_, ok := flatOCRReadableExt[ext]
		return ok
	case engineStructured:
		_, denied := structuredUnreadableExt[ext]
		return !denied
	default:
		return false
	}
}

// extractorSupportsExt reports whether the selected document extractor can
// actually read the given file extension (#394), consulting the consolidated
// capability table above. SPEC §7.4.B routes every pdf/image/document doc_type
// to one configured extractor, but no single extractor reads every format in
// those coarse buckets, so a format outside the active engine's set must be
// skipped with a visible diagnostic rather than handed to an extractor that
// fails silently or hard-errors on it.
//
// `structured` is true for the docling family (local CLI or docling-serve — the
// engines that emit a DoclingDocument), false for the flat Mistral-OCR path.
func extractorSupportsExt(structured bool, ext string) bool {
	engine := engineFlatOCR
	if structured {
		engine = engineStructured
	}
	return engineSupportsExt(engine, ext)
}

// ExtractorSupportsExt is the exported wrapper over extractorSupportsExt so
// out-of-package diagnostics (the doctor extraction-coverage check) can consult
// the same single source of truth for which formats the active engine covers,
// without duplicating the allowlist. `structured` mirrors extractorCanReadExt's
// distinction: true for the docling family, false for the flat OCR path.
func ExtractorSupportsExt(structured bool, ext string) bool {
	return extractorSupportsExt(structured, ext)
}

// --- Capability-aware, per-format selection (#395 Stage 2 / #556) --------------
//
// SPEC §7.4.B.1 defines the extraction-engine registry as a fidelity-ordered
// capability matrix and mandates "best-available per format" routing under
// `ingest.extractor: auto`: for each classified document, select the ACTIVE
// engine of lowest fidelity tier whose matrix cell for that format is ✅, falling
// through the tier order (T1 docling → T2 pandoc[future #393] → T3 mistral OCR →
// T4 raw_text). A format no active engine supports degrades per §7.4.B.2; html is
// the one format whose T4 `raw_text` baseline (§7.4.A) is unconditional, so it is
// never dropped. selectExtractionRoute below is that algorithm; the ingest
// service consumes it (deriving which engines are active from the resolved
// extractor) so routing is a single, spec-faithful decision rather than a coarse
// doc_type switch. pandoc (T2) is declared in the matrix for completeness but is
// never active until #393 ships, so it is not represented as an eligible engine
// here.

// markupReadableExt is the markup (html) format class of the §7.4.B.1 matrix. It
// is the only class with a `raw_text` (T4) ✅ cell, and per §7.4.A that baseline
// is guaranteed: html is never dropped even when no structured engine is active
// and even when a single engine is pinned.
var markupReadableExt = map[string]struct{}{
	".html":  {},
	".htm":   {},
	".xhtml": {},
}

// isMarkupExt reports whether ext is a markup (html) extension eligible for the
// unconditional §7.4.A raw_text baseline. ext is expected lowercased with its
// leading dot.
func isMarkupExt(ext string) bool {
	_, ok := markupReadableExt[ext]
	return ok
}

// extractionRoute is the pipeline action the per-format selection resolves for a
// document's format (§7.4.B.1). The ingest service consumes it to decide how — or
// whether — to produce an extracted_markdown representation.
type extractionRoute int

const (
	// routeDegrade: no active engine supports the format (a coverage gap under
	// `auto`, or a pinned engine that cannot read it). Handled per the §7.4.B.2
	// strict/lenient degradation contract; never a silent empty representation.
	routeDegrade extractionRoute = iota
	// routeStructured: the docling family (T1) — structured extracted_markdown
	// with region spans (§7.4.B "Structured extraction").
	routeStructured
	// routeFlatOCR: the mistral OCR engine (T3) — page-separated extracted_markdown
	// (§7.4.B "Page-separated extraction").
	routeFlatOCR
	// routeRawText: the T4 raw_text baseline (§7.4.A). Applies only to markup
	// (html) and is the guaranteed fallback so html is never dropped.
	routeRawText
)

// extractionAvailability records which extraction engines are ACTIVE for the run
// (§7.4.B "Extractor availability"). It is derived once from the resolved
// extractor so the per-format selection can never drift from the engine the
// service will actually run.
type extractionAvailability struct {
	// structured is true when the docling family (local CLI or docling-serve) is
	// active — it resolved and passed its availability probe.
	structured bool
	// flatOCR is true when the mistral OCR engine (the active `ocr` provider) is
	// available.
	flatOCR bool
}

// selectExtractionRoute implements SPEC §7.4.B.1's best-available per-format
// selection plus the §7.4.A markup boundary (#556). Given the `ingest.extractor`
// policy, the set of active engines, and a lowercased file extension, it returns
// the highest-fidelity ACTIVE engine that supports the format, falling through
// the fidelity order. The selection is deterministic (a pure function of its
// inputs) and, because availability is resolved once per run, cached for the run.
//
//   - `auto`: every engine is eligible (subject to availability).
//   - `docling` / `docling-serve`: only the structured (docling family) engine is
//     eligible — a pinned engine gets no cross-engine fallback (§7.4.B.1).
//   - `mistral`: only the flat OCR engine is eligible.
//   - `off`: no extraction engine is eligible; html still falls to its raw_text
//     baseline, and pdf/image/document simply produce no extracted representation.
//
// A format no eligible+active engine supports returns routeDegrade, EXCEPT markup
// (html), whose raw_text baseline (§7.4.A) is unconditional and exempt from the
// pin restriction so html is never dropped.
func selectExtractionRoute(policy string, avail extractionAvailability, ext string) extractionRoute {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = "auto"
	}
	structuredAllowed := policy == "auto" || policy == "docling" || policy == "docling-serve"
	flatAllowed := policy == "auto" || policy == "mistral"

	// T1 docling (structured): reads every extraction format except the
	// structured-unreadable denylist.
	if structuredAllowed && avail.structured && engineSupportsExt(engineStructured, ext) {
		return routeStructured
	}
	// T3 mistral OCR (flat): the pdf/png/jpg/jpeg/webp allowlist.
	if flatAllowed && avail.flatOCR && engineSupportsExt(engineFlatOCR, ext) {
		return routeFlatOCR
	}
	// T4 raw_text baseline: markup (html) only, always available and exempt from
	// the pin restriction (§7.4.A: "raw_text remains the guaranteed baseline;
	// HTML is never dropped, and behavior MUST NOT regress when docling is absent").
	if isMarkupExt(ext) {
		return routeRawText
	}
	return routeDegrade
}
