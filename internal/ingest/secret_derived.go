package ingest

import (
	"context"
	"errors"
	"os"
	"regexp"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Derived-text secret screening (dir2mcp #681).
//
// SPEC §7.2 applies security.secret_patterns to file CONTENTS, and §15.2 records
// the outcome as the closed skip_reason `secret_excluded`. Ingest honored that
// for one kind of content only: the raw bytes of the source file. A credential
// that reaches the index through a DERIVED text was never tested against the
// patterns at all, so it was chunked, embedded, and served by `search` and `ask`.
//
// Three ordinary corpora hit this:
//
//   - a scanned contract or a screenshot: the key is pixels in the source and
//     plain text only after OCR;
//   - a recorded standup or a support call: the key is audio in the source and
//     plain text only after transcription;
//   - a .docx or .pdf: the key is inside a compressed container in the source and
//     plain text only after extraction.
//
// The same applies to a translated transcript, a recognition statement, a
// subtitle sidecar's cues, and a stored annotation. Every one of them is text
// this daemon creates and then makes searchable, so every one of them is scanned
// here BEFORE it is persisted.
//
// The verdict is document-wide, not representation-wide. A document is a single
// unit of disclosure: leaving its clean representations searchable next to a
// withheld one would still let a caller pull the surrounding context of a
// credential, and would report a partially indexed document as healthy. So the
// first derived match withholds the whole document as `secret_excluded` and
// retires anything this run already wrote for it.
//
// Nothing here logs, persists, or returns the matched text, the matching pattern,
// or an offset. The operator learns WHICH document was withheld and that a
// configured pattern matched; the payload never leaves the process.

// Derived-text kinds, used only for the operator-facing log line and to name the
// producer in review. They are diagnostic labels, not a persisted vocabulary:
// SPEC §15.2 closes the skip_reasons enum, and every one of these withholds the
// document under the SAME `secret_excluded` reason the raw-byte path uses.
const (
	derivedKindExtraction  = "extraction"
	derivedKindTranscript  = "transcript"
	derivedKindTranslation = "translation"
	derivedKindRecognition = "recognition"
	derivedKindSidecar     = "subtitle sidecar"
)

// ErrSecretExcluded reports that derived text was withheld because it matched a
// configured security.secret_patterns entry. It carries no part of the text, the
// pattern, or the offset, so it is safe to surface verbatim on a tool or
// diagnostic surface. `dir2mcp_annotate` maps it to the canonical §14.2
// FORBIDDEN ("path/content blocked by policy").
var ErrSecretExcluded = errors.New("content withheld: it matched a configured security.secret_patterns entry")

// beginDocumentSecretScope opens the per-document secret scope: it captures the
// compiled pattern set the caller is using for the raw-byte scan and clears the
// derived-match flag. Called at every document entry point so a document can
// never inherit the previous document's verdict, and so the derived scan can
// never run against a different pattern set than the source scan.
func (s *Service) beginDocumentSecretScope(patterns []*regexp.Regexp) {
	s.docSecretPatterns = patterns
	s.secretExcludedThisDoc = false
}

// screenDerivedSecrets reports whether derived text must be withheld because it
// matches a configured secret pattern. It returns false (the common case) when
// no patterns are configured, the text is empty, or nothing matches, so a caller
// reads as `if s.screenDerivedSecrets(...) { return }` and otherwise proceeds
// exactly as before.
//
// A true return means the caller MUST NOT persist the representation, its chunks,
// its title, or any other trace of the text. The document has already been
// withheld by the time it returns.
//
// It is idempotent per document: several derived texts of one document (a
// transcript plus its per-language translations) withhold it once.
func (s *Service) screenDerivedSecrets(ctx context.Context, doc model.Document, kind, text string) bool {
	if !s.derivedTextHasSecret(text) {
		return false
	}
	s.withholdDocumentForSecret(ctx, doc, kind)
	return true
}

// derivedTextHasSecret is the pure predicate behind screenDerivedSecrets: it
// tests derived text against the document's active pattern set and does nothing
// else. Use it where a match must refuse ONE output without withholding the whole
// document; use screenDerivedSecrets everywhere the derived text belongs to the
// document's own pipeline.
func (s *Service) derivedTextHasSecret(text string) bool {
	if text == "" || len(s.docSecretPatterns) == 0 {
		return false
	}
	return hasSecretMatch([]byte(text), s.docSecretPatterns)
}

// withholdDocumentForSecret records the document as `secret_excluded` and takes
// back anything this run already made searchable for it.
//
// Order matters. The representations are retired FIRST, so a crash between the
// two steps leaves a document with no searchable chunks rather than a document
// whose row says "withheld" while its chunks are still being served.
//
// The content_hash is taken from the caller's document value and is NOT rewritten
// here, because the two callers need opposite things and each already holds the
// right value:
//
//   - on the single-pass path the #402 done marker is still withheld (empty), so
//     an ungraceful death before the finalize step leaves the document to be
//     reprocessed rather than settled half-retired. processDocument stamps the
//     marker afterwards through finalizeSecretExcluded.
//   - on the two-phase derivation pass the marker was already stamped by the
//     transcription pass, and it must STAY stamped: blanking it would send the
//     next run's transcription pass through the full pipeline again, republish the
//     clean transcript, and only withhold the document once the derivation pass
//     caught up. The document would be searchable in between.
//
// The row's title and error_message are cleared deliberately. A title lifted from
// a document that is being withheld is itself derived text from that document, and
// error_message is a diagnostic surface (§15.6) that must carry nothing from it.
func (s *Service) withholdDocumentForSecret(ctx context.Context, doc model.Document, kind string) {
	if s.secretExcludedThisDoc {
		return
	}
	s.secretExcludedThisDoc = true

	// Content-free by construction: the document path, the producer label, and the
	// fact that a configured pattern matched. No offset, no pattern, no payload.
	s.getLogger().Printf(
		"secret policy: withholding %s; its %s output matched a configured security.secret_patterns entry, so the document is recorded secret_excluded and is not searchable",
		doc.RelPath, kind,
	)

	s.retireDocumentRepresentations(ctx, doc.RelPath)

	doc.Status = "secret_excluded"
	doc.SkipReason = model.SkipReasonSecretExcluded
	doc.Title = ""
	doc.ErrorMessage = ""
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("secret policy: record secret_excluded for %s failed: %v", doc.RelPath, err)
	}

	s.creditSecretExcludedSkip(doc)
}

// creditSecretExcludedSkip accounts for a withheld document as the skip it now
// is (issue #426). The document was built as "ok", so its indexed credit is
// still pending and is dropped by the secretExcludedThisDoc branch in
// processDocument; counting the skip here keeps scanned = indexed + skipped +
// errors and emits the one file_skip event SPEC §3.2 ties to a terminal skip.
//
// Two cases are deliberately not counted, and both would otherwise push the
// run's totals past its scanned count:
//
//   - an archive MEMBER. Members are never credited to the run's scanned/skipped
//     counters at all; the container document accounts for the archive. This
//     matches persistArchiveMemberSizeCapSkips, which persists a member's skip row
//     without reporting it either.
//   - the two-phase DERIVATION pass. scanPass counts the corpus on the first pass
//     only, so the derivation pass already counted this asset as scanned once.
//
// Each case still writes its own `secret_excluded` row, so the honest-coverage
// aggregate (§7.7 skip_reasons) reports it either way.
func (s *Service) creditSecretExcludedSkip(doc model.Document) {
	if doc.SourceType == "archive_member" || s.activePass == passDerivation {
		return
	}
	s.addSkipped(1)
	s.markActiveSkipped()
	s.notifyDocumentSkip(doc)
}

// retireDocumentRepresentations tombstones every live representation of a
// document together with its chunks, which is what removes them from retrieval
// (§6.6: SQLite `deleted = 1` is the source of truth a vector hit is tested
// against, so the vectors disappear from the running session and stay gone after
// a restart).
//
// It reuses the #692 reconciliation store surface rather than adding a second
// delete path. Each producer screens its own text BEFORE persisting it, so in the
// ordinary case there is nothing to retire; this covers the document that had a
// clean representation written earlier in the SAME run (a clean transcript
// followed by a translation that carries the credential).
//
// Best-effort with a loud log, never fatal: a store that cannot retire must not
// convert a withheld document into a failed run. The document row is still
// recorded `secret_excluded` either way.
func (s *Service) retireDocumentRepresentations(ctx context.Context, relPath string) {
	s.retireRepresentationsForPath(ctx, relPath, "secret policy")
}

// retireRepresentationsForPath is the shared retirement step: it tombstones every
// live representation of relPath together with its chunks, and labels its logs
// with the policy that asked for it.
//
// Two policies retire a document's representations, and both need the same
// action. The secret policy (#681) withholds a document whose derived text
// carries a credential. The size cap (#682) refuses a document whose bytes passed
// the cap, which retires anything an earlier scan indexed from the smaller
// version. A second delete path for the second policy would be a second place for
// the §6.6 tombstone rule to be got wrong, so there is one.
func (s *Service) retireRepresentationsForPath(ctx context.Context, relPath, policy string) {
	lister, listOK := s.store.(activeRepresentationLister)
	retirer, retireOK := s.store.(representationRetirer)
	if !listOK || !retireOK {
		s.getLogger().Printf(
			"%s: store cannot retire representations for %s; verify no stale representation is left for it",
			policy, relPath,
		)
		return
	}
	reps, err := lister.ActiveRepresentations(ctx, relPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.getLogger().Printf("%s: list representations for %s failed: %v", policy, relPath, err)
		}
		return
	}
	if len(reps) == 0 {
		return
	}
	ids := make([]int64, 0, len(reps))
	for _, rep := range reps {
		ids = append(ids, rep.RepID)
	}
	if _, err := retirer.SoftDeleteRepresentations(ctx, relPath, ids); err != nil {
		s.getLogger().Printf("%s: retire representations for %s failed: %v", policy, relPath, err)
	}
}

// carrySecretExclusion makes a DERIVED secret verdict stick across scans.
//
// The raw-byte verdict is reproducible: buildDocumentWithContent re-reads the
// same bytes every run and re-reaches the same answer. A derived verdict is not.
// It comes from OCR, STT, or extraction output that the incremental gate is
// designed NOT to recompute for an unchanged file, so without this the next scan
// would rebuild the document as "ok", write that status over the withheld row,
// and then early-return on the unchanged-content gate. The document would be
// reported as indexed while its representations stayed retired.
//
// The carry is conditional on the content being provably unchanged: the stored
// row must be `secret_excluded` AND carry a settled (non-empty) content_hash that
// equals the one just computed from the source. An edited file, or a `--force`
// reindex (which clears the stored hash), therefore re-derives and decides again
// from scratch, so a credential removed from a document lets it back into the
// index. The title is cleared for the same reason it is cleared on the withhold
// path: it is derived text from a withheld document.
func carrySecretExclusion(doc *model.Document, existing model.Document) {
	if doc.Status != "ok" || existing.Status != "secret_excluded" {
		return
	}
	if existing.ContentHash == "" || existing.ContentHash != doc.ContentHash {
		return
	}
	doc.Status = "secret_excluded"
	doc.SkipReason = model.SkipReasonSecretExcluded
	doc.Title = ""
}

// finalizeSecretExcluded stamps the withheld #402 done marker onto a document
// that a derived text withheld, so an unchanged file settles instead of being
// re-derived on every scan. It is the `secret_excluded` counterpart of
// finalizeIfGenerated and inherits the #413 guard: the marker is stamped only if
// the freshly re-read row is STILL `secret_excluded`, so a status another path
// wrote out of band is never resurrected into a done state.
func (s *Service) finalizeSecretExcluded(ctx context.Context, doc *model.Document, contentHash string) error {
	return s.finalizeContentHashForStatus(ctx, doc, contentHash, "secret_excluded")
}
