package ingest

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
