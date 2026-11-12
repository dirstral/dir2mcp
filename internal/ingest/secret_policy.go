package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
//     `ok` document, so its derived text is never screened again, and an archive
//     member is never re-read while its container is unchanged.
//
// SPEC §7.2 requires both directions on the first scan after a change. Both use
// the same rule: clear the content hash of each document the change can affect,
// before the scan visits any file. An empty content hash is the existing "not
// finalized" state, so the normal pipeline re-reads the source and re-derives
// the text, and the same code that decided the first verdict decides again.
// Nothing is reconstructed from stored chunks: a chunk can drop bytes the raw
// scan sees (a trimmed trailing newline, the markup of a member), and two chunks
// can overlap, so a check over chunk text is not the check ingest makes.
// OCR, transcription and translation come from their on-disk caches, so the
// re-read does not repeat a provider call that already succeeded.
//
//  1. Withheld documents are always evaluated again: every `secret_excluded`
//     document, and every archive that holds one.
//  2. Indexed documents are evaluated again only when the new set can match
//     something the old set did not. The new set is NARROWER when each of its
//     patterns is in the old set, or is a declared narrowing of an old pattern
//     (narrowedSecretPatterns). Then no `ok` document can start to match, and
//     the cost of direction 2 (a read of every indexed document) is avoided.
//     Otherwise every `ok` document and every archive is cleared.
//
// The reset and the new identity commit in one store transaction. The retry
// state is the empty hash on each row: a document that fails, or that a stopped
// scan did not reach, keeps it and is processed again on a later scan. A failure
// of the reset itself fails the scan, so it is retried in full.

// preSpec0930SecretPatterns is the default list every release before spec
// 0.93.0 shipped (since #443). A corpus without a recorded policy was indexed
// by such a release, so this list stands in for its unrecorded policy.
var preSpec0930SecretPatterns = []string{
	`AKIA[0-9A-Z]{16}`,
	`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,
	`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
	`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
	`sk-[A-Za-z0-9-]{20,}`,
	`[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}`,
}

// narrowedSecretPatterns maps a pattern to the older pattern it narrows: every
// string the new pattern matches, the old one matches too. Each new pattern
// keeps the old pattern's core as a literal suffix and only adds a left
// boundary or a key-name prefix, so this holds by construction (spec 0.93.0).
var narrowedSecretPatterns = map[string]string{
	`(?i)(?:^|[^a-z0-9_-])(?:(?:[a-z0-9]+[_-])*(?:access|auth|api|app|bearer|secret|private|password|reset|session|refresh|personal|client|oauth|service|account|admin|deploy|webhook|install|invite|vault|ci|job|runner|registry|github|gh|gitlab|slack|npm|pypi|hf|bot)[_-]?)?token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`: `(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
	`(?:^|[^A-Za-z0-9_])(?:sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32})`: `sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
	`(?:^|[^A-Za-z0-9_-])sk-[A-Za-z0-9-]{20,}`:                   `sk-[A-Za-z0-9-]{20,}`,
}

// secretPolicySources returns the sorted pattern strings of a compiled set.
func secretPolicySources(patterns []*regexp.Regexp) []string {
	sources := make([]string, 0, len(patterns))
	for _, rx := range patterns {
		if rx == nil {
			continue
		}
		sources = append(sources, rx.String())
	}
	sort.Strings(sources)
	return sources
}

// secretPolicyIdentity is the fingerprint of the effective pattern set. The
// order of the patterns does not change a verdict (any match withholds), so the
// pattern strings are sorted before hashing; an operator who reorders the list
// does not trigger a pass. The sorted list is JSON-encoded before hashing, so
// the encoding is unambiguous: a pattern that contains a newline can never hash
// the same as two patterns.
func secretPolicyIdentity(patterns []*regexp.Regexp) string {
	// Marshalling a []string cannot fail.
	encoded, _ := json.Marshal(secretPolicySources(patterns))
	sum := sha256.Sum256(encoded)
	return "v1:" + hex.EncodeToString(sum[:])
}

// secretPolicyIsNarrower reports whether every pattern of active is in
// previous or is a declared narrowing of a pattern in previous. Then a document
// that previous did not match cannot match active.
func secretPolicyIsNarrower(previous, active []string) bool {
	known := make(map[string]struct{}, len(previous))
	for _, p := range previous {
		known[p] = struct{}{}
	}
	for _, p := range active {
		if _, ok := known[p]; ok {
			continue
		}
		if old, ok := narrowedSecretPatterns[p]; ok {
			if _, had := known[old]; had {
				continue
			}
		}
		return false
	}
	return true
}

// secretPolicyStore is the optional store capability behind the re-evaluation.
// A store without it keeps the previous behaviour.
type secretPolicyStore interface {
	SecretPolicyIdentity(ctx context.Context) (string, error)
	SecretPolicyPatterns(ctx context.Context) ([]string, bool, error)
	ResetForSecretPolicy(ctx context.Context, identity string, patterns []string, reevaluateIndexed bool) (int64, error)
}

// beginSecretPolicyReevaluation compares the recorded secret policy with the
// active one and, when they differ, resets the documents the change can affect
// (see the file comment). It runs once per scan, before any document is
// visited. A store failure is returned and fails the scan: a scan that could
// not apply a changed policy must not report success, and the next scan tries
// again because nothing was recorded.
func (s *Service) beginSecretPolicyReevaluation(ctx context.Context, patterns []*regexp.Regexp) error {
	policyStore, ok := s.store.(secretPolicyStore)
	if !ok {
		return nil
	}
	active := secretPolicyIdentity(patterns)
	recorded, err := policyStore.SecretPolicyIdentity(ctx)
	if err != nil {
		return fmt.Errorf("secret policy: read the recorded security.secret_patterns identity: %w", err)
	}
	if recorded == active {
		return nil
	}
	sources := secretPolicySources(patterns)
	previous, known := preSpec0930SecretPatterns, true
	if recorded != "" {
		previous, known, err = policyStore.SecretPolicyPatterns(ctx)
		if err != nil {
			return fmt.Errorf("secret policy: read the recorded security.secret_patterns list: %w", err)
		}
	}
	reevaluateIndexed := !known || !secretPolicyIsNarrower(previous, sources)
	cleared, err := policyStore.ResetForSecretPolicy(ctx, active, sources, reevaluateIndexed)
	if err != nil {
		return fmt.Errorf("secret policy: reset documents for the changed security.secret_patterns: %w", err)
	}
	scope := "withheld documents"
	if reevaluateIndexed {
		scope = "withheld and indexed documents"
	}
	s.getLogger().Printf(
		"secret policy: security.secret_patterns changed since the last scan; %d %s (and the archives that hold them) will be read and evaluated again",
		cleared, scope)
	return nil
}

// retireNewlyWithheld retires the representations of a document that its raw
// bytes now withhold while its stored row is still `ok`. That happens when the
// pattern set became broader (SPEC §7.2): the raw-byte scan reaches the new
// verdict on the read, but the document generates no new representations, so
// without this its old chunks stay searchable under a `secret_excluded` row.
//
// It runs BEFORE the caller upserts the new status, and a failure is returned so
// the caller does not write that status: the row stays `ok` with its current
// content hash, and the next read decides again. Writing `secret_excluded` over
// live chunks would hide the document from this check for good, because the
// check fires only on a row that is still `ok`.
func (s *Service) retireNewlyWithheld(ctx context.Context, existing, doc model.Document) error {
	if doc.Status != string(model.DocumentStatusSecretExcluded) {
		return nil
	}
	if existing.RelPath == "" || existing.Deleted || existing.Status != string(model.DocumentStatusOK) {
		return nil
	}
	if err := s.retireRepresentationsForPath(ctx, doc.RelPath, "secret policy"); err != nil {
		return fmt.Errorf("secret policy: retire the representations of newly withheld %s: %w", doc.RelPath, err)
	}
	return nil
}

// settleSecretVerdict applies the two secret-policy steps that run between the
// build of a document row and its upsert: the carry of a derived verdict
// (carrySecretExclusion, #681) and the retirement of an `ok` document that its
// raw bytes now withhold (retireNewlyWithheld). One call keeps processDocument
// and processDocumentFromContent within the complexity budget.
func (s *Service) settleSecretVerdict(ctx context.Context, doc *model.Document, existing model.Document) error {
	carrySecretExclusion(doc, existing)
	return s.retireNewlyWithheld(ctx, existing, *doc)
}

// existingDocWithSecretVerdict reads the stored row of doc and applies
// settleSecretVerdict to doc. It returns the stored row (zero when there is
// none) for the caller's gate and transition log.
func (s *Service) existingDocWithSecretVerdict(ctx context.Context, doc *model.Document) (model.Document, error) {
	existing, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if isUnexpectedStoreErr(err) {
		return model.Document{}, fmt.Errorf("get existing document: %w", err)
	}
	if err := s.settleSecretVerdict(ctx, doc, existing); err != nil {
		return model.Document{}, err
	}
	return existing, nil
}
