package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Secret-policy re-evaluation (SPEC §7.2, spec 0.93.0).
//
// A `secret_excluded` verdict, and equally an `ok` verdict, is reached under one
// set of security.secret_patterns. The effective set changes when a release
// changes a default (0.93.0 narrowed three) or when an operator edits the list.
// The incremental scan does not ask again on its own for an unchanged document:
//
//   - the cheap-signal fast path (ETag, or size and mtime for media) skips the
//     document before any read;
//   - carrySecretExclusion keeps a derived verdict (OCR, STT, extraction) on the
//     row for as long as the content hash is unchanged;
//   - the content gate does not regenerate the representations of an unchanged
//     `ok` document, so a derived text is never screened again.
//
// SPEC §7.2 requires both directions on the first scan after a change:
//
//  1. Each withheld document is evaluated again. At the start of the scan the
//     content hash of every `secret_excluded` document, and of every archive
//     that holds one, is cleared. An empty content hash is the existing "not
//     finalized" state, so the normal pipeline re-reads those documents and
//     decides under the current patterns. A document the new set does not match
//     is indexed.
//  2. Each indexed document that the new set matches is withheld. After the
//     scan, a sweep reads the searchable text of every `ok` document (its live
//     chunks, which hold the raw and the derived text alike) and tests it. A
//     match retires the representations and chunks and records the document
//     `secret_excluded`. The sweep reads the store only: it never calls an
//     extractor or a provider.
//
// The change is detected with one fingerprint of the effective pattern set,
// recorded in the settings table. It is recorded only after both halves
// completed without a failure, so an interrupted or failed pass runs again on
// the next scan. A steady-state scan costs one settings read.

// secretPolicyIdentity is the fingerprint of the effective pattern set. The
// order of the patterns does not change a verdict (any match withholds), so the
// pattern strings are sorted before hashing; an operator who reorders the list
// does not trigger a pass. The sorted list is JSON-encoded before hashing, so
// the encoding is unambiguous: a pattern that contains a newline can never hash
// the same as two patterns.
func secretPolicyIdentity(patterns []*regexp.Regexp) string {
	sources := make([]string, 0, len(patterns))
	for _, rx := range patterns {
		if rx == nil {
			continue
		}
		sources = append(sources, rx.String())
	}
	sort.Strings(sources)
	// Marshalling a []string cannot fail.
	encoded, _ := json.Marshal(sources)
	sum := sha256.Sum256(encoded)
	return "v1:" + hex.EncodeToString(sum[:])
}

// secretPolicyStore is the optional store capability behind the re-evaluation.
// A store without it keeps the previous behaviour.
type secretPolicyStore interface {
	SecretPolicyIdentity(ctx context.Context) (string, error)
	SetSecretPolicyIdentity(ctx context.Context, identity string) error
	ClearSecretExcludedContentHashes(ctx context.Context) (int64, error)
	IndexedDocumentPaths(ctx context.Context) ([]string, error)
	LiveRepresentationTexts(ctx context.Context, relPath string) ([]string, error)
}

// beginSecretPolicyReevaluation compares the recorded secret policy identity
// with the active one. When they differ it arms the post-scan sweep and makes
// every withheld document eligible for a new verdict on this scan (direction 1).
// It is called once per scan, before any document is visited. It never fails
// the scan: a store error is logged, the pass is not armed, and the identity
// stays unrecorded, so the next scan tries again.
func (s *Service) beginSecretPolicyReevaluation(ctx context.Context, patterns []*regexp.Regexp) {
	s.pendingSecretPolicy = ""
	policyStore, ok := s.store.(secretPolicyStore)
	if !ok {
		return
	}
	active := secretPolicyIdentity(patterns)
	recorded, err := policyStore.SecretPolicyIdentity(ctx)
	if err != nil {
		s.getLogger().Printf("secret policy: read secret policy identity failed, withheld and indexed documents keep their verdict this scan: %v", err)
		return
	}
	if recorded == active {
		return
	}
	cleared, err := policyStore.ClearSecretExcludedContentHashes(ctx)
	if err != nil {
		s.getLogger().Printf("secret policy: reset withheld documents failed, withheld and indexed documents keep their verdict this scan: %v", err)
		return
	}
	s.pendingSecretPolicy = active
	s.getLogger().Printf(
		"secret policy: security.secret_patterns changed since the last scan; %d withheld document(s) or archive(s) will be evaluated again, and indexed documents are checked after the scan",
		cleared)
}

// finishSecretPolicyReevaluation runs direction 2 after a completed scan and
// then records the identity. It is a no-op unless this scan armed the pass.
func (s *Service) finishSecretPolicyReevaluation(ctx context.Context, patterns []*regexp.Regexp) {
	active := s.pendingSecretPolicy
	s.pendingSecretPolicy = ""
	if active == "" {
		return
	}
	policyStore, ok := s.store.(secretPolicyStore)
	if !ok {
		return
	}
	withheld, complete := s.sweepIndexedDocumentsForSecrets(ctx, policyStore, patterns)
	if withheld > 0 {
		s.getLogger().Printf("secret policy: withheld %d indexed document(s) that the current security.secret_patterns match", withheld)
	}
	if !complete {
		s.getLogger().Printf("secret policy: a step of the indexed-document check failed; the secret policy identity stays unrecorded so the next scan checks again")
		return
	}
	if err := policyStore.SetSecretPolicyIdentity(ctx, active); err != nil {
		s.getLogger().Printf("secret policy: record secret policy identity failed: %v", err)
	}
}

// sweepIndexedDocumentsForSecrets tests the searchable text of every `ok`
// document against patterns and withholds each match. It returns the number of
// documents it withheld and whether every step succeeded. A context
// cancellation stops it and reports it incomplete.
func (s *Service) sweepIndexedDocumentsForSecrets(ctx context.Context, policyStore secretPolicyStore, patterns []*regexp.Regexp) (int, bool) {
	if len(patterns) == 0 {
		return 0, true
	}
	paths, err := policyStore.IndexedDocumentPaths(ctx)
	if err != nil {
		s.getLogger().Printf("secret policy: list indexed documents failed: %v", err)
		return 0, false
	}
	withheld := 0
	complete := true
	for _, relPath := range paths {
		if ctx.Err() != nil {
			return withheld, false
		}
		texts, err := policyStore.LiveRepresentationTexts(ctx, relPath)
		if err != nil {
			s.getLogger().Printf("secret policy: read searchable text of %s failed: %v", relPath, err)
			complete = false
			continue
		}
		if !anyTextHasSecret(texts, patterns) {
			continue
		}
		if !s.withholdIndexedDocument(ctx, relPath) {
			complete = false
			continue
		}
		withheld++
	}
	return withheld, complete
}

func anyTextHasSecret(texts []string, patterns []*regexp.Regexp) bool {
	for _, text := range texts {
		if text != "" && hasSecretMatch([]byte(text), patterns) {
			return true
		}
	}
	return false
}

// withholdIndexedDocument withholds an `ok` document whose searchable text a
// pattern matches. The representations and chunks are retired FIRST, so a
// crash between the two steps leaves no searchable text rather than a withheld
// row with live chunks. The content_hash stays: the verdict then sticks on the
// next scan through carrySecretExclusion while the content is unchanged, and an
// edited file is decided again. It reports whether both steps succeeded. As on
// every other secret path, nothing of the matched text is logged.
func (s *Service) withholdIndexedDocument(ctx context.Context, relPath string) bool {
	previous, err := s.store.GetDocumentByPath(ctx, relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isNotFoundError(err) {
			return true
		}
		s.getLogger().Printf("secret policy: read %s failed: %v", relPath, err)
		return false
	}
	if previous.Deleted || previous.Status != string(model.DocumentStatusOK) {
		return true
	}
	s.getLogger().Printf(
		"secret policy: withholding %s; its indexed text matched a configured security.secret_patterns entry, so the document is recorded secret_excluded and is not searchable",
		relPath)
	retired := s.retireRepresentationsForPath(ctx, relPath, "secret policy") == nil

	doc := previous
	doc.Status = string(model.DocumentStatusSecretExcluded)
	doc.SkipReason = model.SkipReasonSecretExcluded
	doc.Title = ""
	doc.ErrorMessage = ""
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("secret policy: record secret_excluded for %s failed: %v", relPath, err)
		return false
	}
	s.observeDocumentTransition(previous, doc)
	return retired
}

// retireNewlyWithheld retires the representations of a document that its raw
// bytes now withhold while its stored row is still `ok`. That happens when the
// pattern set became broader (SPEC §7.2): the raw-byte scan reaches the new
// verdict on the next read, but an unchanged document generates no new
// representations, so without this its old chunks stay searchable under a
// `secret_excluded` row. It runs BEFORE the caller upserts the new status, so a
// crash between the two steps leaves no searchable text.
func (s *Service) retireNewlyWithheld(ctx context.Context, existing, doc model.Document) {
	if doc.Status != string(model.DocumentStatusSecretExcluded) {
		return
	}
	if existing.RelPath == "" || existing.Deleted || existing.Status != string(model.DocumentStatusOK) {
		return
	}
	s.retireDocumentRepresentations(ctx, doc.RelPath)
}
